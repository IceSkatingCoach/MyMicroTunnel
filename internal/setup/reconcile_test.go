// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
)

// installedProfile is a deployed profile as the reconcile loop finds it: its
// settings in the store and its configuration in /etc/wireguard.
func installedProfile(t *testing.T, fake *fakeMachine, name, interfaceName string, wantUp bool) superviseTarget {
	t.Helper()
	s := valid()
	s.ProfileName = name
	s.InterfaceName = interfaceName
	s.Supervise = true
	s.ServerPublicKey = "c2VydmVyLXB1YmxpYy1rZXktYnl0ZXMtMzItbG9uZyE="
	s.Endpoint = "203.0.113.10"
	writeProfileForTest(t, s)
	// The key inline rather than in /etc/wireguard, which a test cannot write.
	configuration := strings.Replace(TunnelConfig(s), "[Interface]\n",
		"[Interface]\nPrivateKey = "+s.ServerPublicKey+"\n", 1)
	fake.place(t, s.TunnelConfigPath(), configuration, 0o600)
	if err := SetDesiredState(name, wantUp); err != nil {
		t.Fatal(err)
	}
	return superviseTarget{
		profileName:   name,
		statePath:     s.DesiredStatePath(),
		interfaceName: interfaceName,
		configPath:    s.TunnelConfigPath(),
	}
}

func forgetMissingConfigs(t *testing.T) {
	t.Cleanup(func() { missingConfigLogged = map[string]bool{} })
}

func TestTheSupervisorRaisesATunnelTheUserLeftOn(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", true)

	log := captured(t, func() { reconcileTunnel(target) })

	if len(fake.raised) != 1 {
		t.Fatalf("%d raises, want 1", len(fake.raised))
	}
	raised := fake.raised[0]
	if raised.Name != "wg2" || raised.Address != "10.100.0.2" || raised.MTU != TunnelMTU {
		t.Errorf("raised %+v, want wg2 at 10.100.0.2 with the tunnel MTU", raised)
	}
	if !strings.Contains(log, "bringing wg2 up") {
		t.Errorf("the log does not say what it did:\n%s", log)
	}
}

func TestTheSupervisorReportsATunnelThatWouldNotComeUp(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", true)
	fake.upErr = errors.New("no utun available")

	log := captured(t, func() { reconcileTunnel(target) })

	if !strings.Contains(log, "could not raise wg2: no utun available") {
		t.Errorf("the failure was not logged:\n%s", log)
	}
}

// Switching the tunnel off is a decision, not a fault to be repaired.
func TestTheSupervisorDropsATunnelTheUserSwitchedOff(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", false)
	fake.devices["wg2"] = "utun7"
	fake.downErr = errors.New("busy")

	log := captured(t, func() { reconcileTunnel(target) })

	if len(fake.dropped) != 1 || fake.dropped[0] != "wg2" {
		t.Errorf("dropped %v, want [wg2]", fake.dropped)
	}
	if len(fake.raised) != 0 {
		t.Errorf("a tunnel switched off was raised: %+v", fake.raised)
	}
	if !strings.Contains(log, "could not drop wg2: busy") {
		t.Errorf("the failure to drop was not logged:\n%s", log)
	}
}

func TestTheSupervisorLeavesAHealthyTunnelAlone(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", true)
	fake.devices["wg2"] = "utun7"
	fake.status["wg2"] = tunnel.Status{Peers: []tunnel.PeerStatus{{LastHandshake: time.Now().Add(-10 * time.Second)}}}

	captured(t, func() { reconcileTunnel(target) })

	if len(fake.raised) != 0 || len(fake.dropped) != 0 {
		t.Errorf("a connected tunnel was bounced: raised %v, dropped %v", fake.raised, fake.dropped)
	}
}

// A tunnel whose peer went quiet is re-pinned: dropped and raised again, so a
// gateway that changed address is found again.
func TestTheSupervisorRepinsATunnelWhosePeerWentQuiet(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", true)
	fake.devices["wg2"] = "utun7"
	fake.status["wg2"] = tunnel.Status{Peers: []tunnel.PeerStatus{{LastHandshake: time.Now().Add(-time.Hour)}}}

	log := captured(t, func() { reconcileTunnel(target) })

	if len(fake.dropped) != 1 || len(fake.raised) != 1 {
		t.Fatalf("dropped %v and raised %v, want one of each", fake.dropped, fake.raised)
	}
	if !strings.Contains(log, "no handshake on wg2") {
		t.Errorf("the re-pin was not explained:\n%s", log)
	}
	// No idle timeout, so there is nothing to wake and nothing to run as the user.
	if fake.ran("/usr/bin/sudo -u") {
		t.Error("a gateway with no idle timeout was woken")
	}
}

func TestARepinStopsWhenTheTunnelWillNotDrop(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", true)
	fake.devices["wg2"] = "utun7"
	fake.status["wg2"] = tunnel.Status{Peers: []tunnel.PeerStatus{{}}}
	fake.downErr = errors.New("busy")

	log := captured(t, func() { reconcileTunnel(target) })

	if len(fake.raised) != 0 {
		t.Errorf("raised over a tunnel that was still up: %+v", fake.raised)
	}
	if !strings.Contains(log, "could not drop wg2 before re-pinning: busy") {
		t.Errorf("the failure was not logged:\n%s", log)
	}
}

func TestAFailedRepinIsLogged(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, "work", "wg2", true)
	fake.devices["wg2"] = "utun7"
	fake.status["wg2"] = tunnel.Status{Peers: []tunnel.PeerStatus{{}}}
	fake.upErr = errors.New("no utun available")

	log := captured(t, func() { reconcileTunnel(target) })

	if !strings.Contains(log, "re-pin failed: no utun available") {
		t.Errorf("the failed re-pin was not logged:\n%s", log)
	}
}

// Asking the tunnel how it is and getting no answer is not evidence that it is
// dead. Bouncing on it would make a working tunnel flap.
func TestAnUnreadableTunnelIsNotStale(t *testing.T) {
	fake := stubMachine(t)
	fake.statusErr = errors.New("permission denied")

	var stale bool
	log := captured(t, func() { stale = handshakeIsStale("wg2") })

	if stale {
		t.Error("a tunnel that could not be read was called stale")
	}
	if !strings.Contains(log, "could not read wg2: permission denied") {
		t.Errorf("the failure to read was not logged:\n%s", log)
	}
}

func TestATunnelWithNoPeersIsNotStale(t *testing.T) {
	stubMachine(t)
	if handshakeIsStale("wg2") {
		t.Error("a tunnel with nobody to talk to was called stale")
	}
}

// Between deploying a profile and authorising the privileged step there is no
// configuration to raise. Saying so every fifteen seconds fills the log with
// one line that never changes.
func TestAMissingConfigurationIsReportedOnce(t *testing.T) {
	fake := stubMachine(t)
	forgetMissingConfigs(t)
	target := installedProfile(t, fake, "work", "wg2", true)
	if err := os.Remove(onDisk(target.configPath)); err != nil {
		t.Fatal(err)
	}

	log := captured(t, func() {
		reconcileTunnel(target)
		reconcileTunnel(target)
	})

	if count := strings.Count(log, "has no configuration"); count != 1 {
		t.Errorf("the missing configuration was reported %d times, want once:\n%s", count, log)
	}
	if len(fake.raised) != 0 {
		t.Errorf("a tunnel with no configuration was raised: %+v", fake.raised)
	}

	fake.place(t, target.configPath, "", 0o600)
	captured(t, func() { reconcileTunnel(target) })
	if missingConfigLogged["work"] {
		t.Error("a configuration that appeared is still remembered as missing")
	}
}

func TestTheSingleProfileSupervisorWatchesTheFileItWasGiven(t *testing.T) {
	fake := stubMachine(t)
	target := installedProfile(t, fake, DefaultProfileName, "wg0", true)

	log := captured(t, func() {
		Supervise(SuperviseOptions{
			StatePath:     target.statePath,
			InterfaceName: "wg0",
			ConfigPath:    target.configPath,
			Once:          true,
		})
	})

	if !strings.Contains(log, "supervisor started for wg0, watching "+target.statePath) {
		t.Errorf("the start was not logged:\n%s", log)
	}
	if len(fake.raised) != 1 || fake.raised[0].Name != "wg0" {
		t.Errorf("raised %+v, want wg0", fake.raised)
	}
}

// --- waking a switched-off gateway -----------------------------------------

func wakeableProfile(t *testing.T, fake *fakeMachine, username string) superviseTarget {
	t.Helper()
	target := installedProfile(t, fake, "work", "wg2", true)
	s, err := LoadProfileSettings("work")
	if err != nil {
		t.Fatal(err)
	}
	s.IdleTimeoutMinutes = 30
	s.GatewayGroupName = "gateway-group"
	s.Username = username
	if err := SaveProfileSettings(s); err != nil {
		t.Fatal(err)
	}
	return target
}

// The daemon is root, and root has no AWS profile; the wake runs as the user
// who owns the deployment.
func TestTheGatewayIsWokenAsTheUserWhoOwnsIt(t *testing.T) {
	fake := stubMachine(t)
	target := wakeableProfile(t, fake, "alice")
	fake.place(t, SupervisorExecutable, "", 0o755)
	fake.reply("/usr/bin/sudo -u alice", "")

	log := captured(t, func() { wakeGateway(target) })

	if !fake.ran("/usr/bin/sudo -u alice " + SupervisorExecutable + " wake --vpn-profile work --quiet") {
		t.Errorf("the wake was not run as alice through the helper: %v", fake.commands)
	}
	if !strings.Contains(log, "waking the gateway for work") {
		t.Errorf("the wake was not logged:\n%s", log)
	}
}

func TestTheWakeUserTheDaemonWasGivenWins(t *testing.T) {
	fake := stubMachine(t)
	target := wakeableProfile(t, fake, "alice")
	target.wakeUser = "bob"
	fake.refuse("/usr/bin/sudo -u bob", "  ExpiredToken  ")

	log := captured(t, func() { wakeGateway(target) })

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !fake.ran("/usr/bin/sudo -u bob " + running + " wake") {
		t.Errorf("without an installed helper the running binary should wake as bob: %v", fake.commands)
	}
	if !strings.Contains(log, "could not wake the gateway for work: ExpiredToken") {
		t.Errorf("the failed wake was not logged:\n%s", log)
	}
}

// Reading the user's AWS profile as root is exactly what the wake must not do.
func TestTheGatewayIsNotWokenAsRoot(t *testing.T) {
	for _, username := range []string{"", "root"} {
		fake := stubMachine(t)
		target := wakeableProfile(t, fake, username)

		log := captured(t, func() { wakeGateway(target) })

		if fake.ran("/usr/bin/sudo") {
			t.Errorf("user %q: a wake was attempted: %v", username, fake.commands)
		}
		if !strings.Contains(log, "not waking work: no unprivileged user") {
			t.Errorf("user %q: the refusal was not logged:\n%s", username, log)
		}
	}
}

func TestAProfileWithoutSettingsIsNotWoken(t *testing.T) {
	fake := stubMachine(t)
	wakeGateway(superviseTarget{profileName: "nowhere"})
	if len(fake.commands) != 0 {
		t.Errorf("a profile with no settings ran %v", fake.commands)
	}
}

// --- raising -----------------------------------------------------------------

func TestRaisingWithNoPathReadsTheInterfacesOwnConfiguration(t *testing.T) {
	fake := stubMachine(t)
	installedProfile(t, fake, DefaultProfileName, "wg0", true)

	if err := RaiseTunnel("wg0", ""); err != nil {
		t.Fatalf("raising wg0: %v", err)
	}
	if len(fake.raised) != 1 || fake.raised[0].Config.Peers[0].Endpoint != "203.0.113.10:51820" {
		t.Errorf("raised %+v, want the peer from /etc/wireguard/wg0.conf", fake.raised)
	}
}

func TestRaisingATunnelWithNoConfigurationFails(t *testing.T) {
	fake := stubMachine(t)
	if err := RaiseTunnel("wg9", ""); err == nil {
		t.Error("a tunnel with no configuration was raised")
	}
	if len(fake.raised) != 0 {
		t.Errorf("raised %+v", fake.raised)
	}
}

// The tunnel claims the VPC range. A machine already on that range would lose
// it — the printer, the router — the moment the tunnel came up.
func TestATunnelThatWouldTakeOverTheLocalNetworkIsRefused(t *testing.T) {
	fake := stubMachine(t)
	installedProfile(t, fake, DefaultProfileName, "wg0", true)
	fake.networks = []tunnel.Network{network(t, "en0", "172.31.4.0/24")}

	err := RaiseTunnel("wg0", "")
	if err == nil || !strings.Contains(err.Error(), "wg0 would route a network this machine is already on") ||
		!strings.Contains(err.Error(), "en0 is already on 172.31.4.0/24") {
		t.Fatalf("the collision was not refused clearly: %v", err)
	}
	if len(fake.raised) != 0 {
		t.Errorf("the tunnel was raised anyway: %+v", fake.raised)
	}
}

// Re-raising a tunnel that is up must not be refused because of the addresses
// it put there itself, nor because of another profile's tunnel.
func TestOurOwnTunnelsAreNotNetworksToProtect(t *testing.T) {
	fake := stubMachine(t)
	installedProfile(t, fake, DefaultProfileName, "wg0", true)
	other := valid()
	other.ProfileName = "home"
	other.InterfaceName = "wg1"
	other.VpnCidr = "10.110.0.0/24"
	other.ClientAddress = "10.110.0.2"
	other.GatewayAddress = "10.110.0.1"
	writeProfileForTest(t, other)
	fake.devices["wg0"] = "utun4"
	fake.networks = []tunnel.Network{
		network(t, "utun4", "172.31.0.0/16"),
		network(t, "utun9", "10.100.0.2/32"),
		network(t, "en0", "192.168.1.0/24"),
	}

	if err := CheckLocalNetworks("wg0", tunnel.File{Address: "10.100.0.2",
		Config: tunnel.Config{Peers: []tunnel.Peer{{AllowedIPs: []string{"10.100.0.0/24", "172.31.0.0/16"}}}}}); err != nil {
		t.Errorf("a tunnel collided with itself: %v", err)
	}
}

func TestAMachineWhoseNetworksCannotBeListedStillRaises(t *testing.T) {
	fake := stubMachine(t)
	fake.networksErr = errors.New("netlink refused")

	var err error
	log := captured(t, func() { err = CheckLocalNetworks("wg0", tunnel.File{Address: "10.100.0.2"}) })

	if err != nil {
		t.Errorf("the tunnel was refused for a machine with bigger problems: %v", err)
	}
	if !strings.Contains(log, "raising wg0 anyway: netlink refused") {
		t.Errorf("the decision was not logged:\n%s", log)
	}
}

func TestADeploymentOnTheLocalNetworkIsRefusedAtInstall(t *testing.T) {
	fake := stubMachine(t)
	s := valid()
	fake.networks = []tunnel.Network{network(t, "en0", "10.100.0.0/16")}

	err := s.ConflictsWithLocalNetworks()
	if err == nil || !strings.Contains(err.Error(), "the tunnel subnet 10.100.0.0/24 is a network this machine is already on") {
		t.Fatalf("the collision was not refused: %v", err)
	}

	fake.networks = []tunnel.Network{network(t, "en0", "192.168.1.0/24")}
	if err := s.ConflictsWithLocalNetworks(); err != nil {
		t.Errorf("a subnet nobody is on was refused: %v", err)
	}

	fake.networksErr = errors.New("netlink refused")
	if err := s.ConflictsWithLocalNetworks(); err != nil {
		t.Errorf("an unreadable machine refused the deployment: %v", err)
	}
}

// --- installing the daemon -------------------------------------------------

func TestTheSupervisorRunsTheRootOwnedHelperWhenThereIsOne(t *testing.T) {
	fake := stubMachine(t)
	fake.place(t, SupervisorExecutable, "", 0o755)
	acceptSupervisor(fake)

	if err := InstallSupervisor("/Users/someone/profiles"); err != nil {
		t.Fatalf("installing the supervisor: %v", err)
	}
	definition := fake.read(t, SupervisorPath)
	if !strings.Contains(definition, SupervisorExecutable) || !strings.Contains(definition, "/Users/someone/profiles") {
		t.Errorf("the definition does not run the helper over the store:\n%s", definition)
	}
	if info, err := os.Stat(onDisk(SupervisorPath)); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("the definition is not mode 644: %v %v", info, err)
	}
}

// A source checkout has no installed helper; the running build is the same
// program.
func TestTheSupervisorRunsThisBuildFromACheckout(t *testing.T) {
	fake := stubMachine(t)
	acceptSupervisor(fake)

	if err := InstallSupervisor("/Users/someone/profiles"); err != nil {
		t.Fatalf("installing the supervisor: %v", err)
	}
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if definition := fake.read(t, SupervisorPath); !strings.Contains(definition, running) {
		t.Errorf("the definition does not name %s:\n%s", running, definition)
	}
}

func TestASupervisorThatCannotBeWrittenIsNotLoaded(t *testing.T) {
	fake := stubMachine(t)
	fake.writeErrors[SupervisorPath] = errors.New("read-only file system")

	if err := InstallSupervisor("/Users/someone/profiles"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("the write failure was not returned: %v", err)
	}
	if len(fake.commands) != 1 {
		t.Errorf("something ran after the write failed: %v", fake.commands)
	}
}

func TestRemovingASupervisorThatWasNeverInstalledDoesNothing(t *testing.T) {
	fake := stubMachine(t)
	RemoveSupervisor(true)
	RemoveSupervisor(false)
	if len(fake.commands) != 0 {
		t.Errorf("ran %v", fake.commands)
	}
}

func TestRemovingTheSupervisorAsRootDeletesItsDefinition(t *testing.T) {
	fake := stubMachine(t)
	fake.place(t, SupervisorPath, "job", 0o644)

	output := captured(t, func() { RemoveSupervisor(true) })

	if _, err := os.Stat(onDisk(SupervisorPath)); !os.IsNotExist(err) {
		t.Errorf("the definition is still there: %v", err)
	}
	if fake.ran("/usr/bin/sudo") {
		t.Errorf("root asked sudo: %v", fake.commands)
	}
	if !strings.Contains(output, "Supervisor removed") {
		t.Errorf("the removal was not reported:\n%s", output)
	}
}

func TestRemovingTheSupervisorUnprivilegedGoesThroughSudo(t *testing.T) {
	fake := stubMachine(t)
	fake.place(t, SupervisorPath, "job", 0o644)

	captured(t, func() { RemoveSupervisor(false) })

	if !fake.ran("/usr/bin/sudo rm -f " + SupervisorPath) {
		t.Errorf("the definition was not removed through sudo: %v", fake.commands)
	}
	if _, err := os.Stat(filepath.Clean(onDisk(SupervisorPath))); err != nil {
		t.Errorf("the unprivileged path deleted the file itself rather than through sudo: %v", err)
	}
}
