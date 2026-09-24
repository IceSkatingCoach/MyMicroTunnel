// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

// The supervisor is the answer to the two things the menu bar switch cannot do:
// survive a reboot, and notice that a tunnel which still has an interface has
// stopped carrying packets.
//
// It is deliberately not "keep the tunnel up". The tunnel is a switch somebody
// turns off on purpose, and a daemon that overrides that is a daemon people
// uninstall. What it keeps is the *last thing the user asked for*, written to a
// file the menu bar can update with no privileges at all:
//
//	~/Library/Application Support/MyMicroTunnel/desired-state   "up" or "down"
//
// Root reads that file, compares it to reality, and reconciles. A laptop that
// reboots with the switch on comes back with the tunnel up; one that reboots
// with the switch off comes back with it down.

const (
	// A handshake older than this means the far end has stopped answering.
	// PersistentKeepalive is 25s, so three minutes is many missed chances
	// rather than one unlucky moment.
	staleHandshake = 180 * time.Second

	// Long enough that a flapping link is not made worse by bouncing the
	// tunnel on top of it, short enough that a wake from sleep recovers before
	// anyone reaches for the menu bar.
	superviseInterval = 15 * time.Second
)

// SetDesiredState records what the user last asked for, for one profile.
// Written by the menu bar and by the installer, both unprivileged.
func SetDesiredState(profileName string, up bool) error {
	if err := os.MkdirAll(ProfileDir(profileName), 0o755); err != nil {
		return err
	}
	state := "down"
	if up {
		state = "up"
	}
	return os.WriteFile(DesiredStatePathFor(profileName), []byte(state+"\n"), 0o644)
}

func desiredUp(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		// No file means nobody has asked for anything yet, and the safe
		// reading of that is "leave the tunnel alone".
		return false
	}
	return strings.TrimSpace(string(content)) == "up"
}

// --- the daemon ------------------------------------------------------------

type SuperviseOptions struct {
	// ProfilesDir is the store the daemon watches. Every profile in it that
	// asks to be supervised is reconciled on each pass, which is what lets one
	// daemon hold several tunnels at the state their owner last chose.
	ProfilesDir string

	// The single-profile spelling, kept because the `supervise` subcommand is
	// also something a person runs by hand when the daemon misbehaves, and
	// because a plist written by an older version still passes these.
	StatePath     string
	InterfaceName string
	ConfigPath    string

	Once bool

	// WakeUser is who the daemon becomes to wake a switched-off gateway. The
	// daemon is root, and root has no AWS profile: the credentials live in the
	// owning user's ~/.aws, and reaching them means dropping to that user
	// rather than reading their files as root.
	WakeUser string
}

// Supervise is the body of the daemon. It reconciles once per interval and
// never exits, because launchd or systemd restarting it is the recovery path for a
// crash rather than something to paper over here.
func Supervise(options SuperviseOptions) {
	if options.ProfilesDir != "" {
		profilesDirOverride = options.ProfilesDir
		logf("supervisor started for every profile under %s", options.ProfilesDir)
	} else {
		logf("supervisor started for %s, watching %s", options.InterfaceName, options.StatePath)
	}

	for {
		for _, target := range superviseTargets(options) {
			reconcileTunnel(target)
		}
		if options.Once {
			return
		}
		time.Sleep(superviseInterval)
	}
}

// superviseTarget is one tunnel to hold at one state. Resolved on every pass
// rather than once at startup, so a profile installed while the daemon is
// running is picked up without a reload — which is the whole reason the daemon
// watches a directory rather than a list baked into its plist.
type superviseTarget struct {
	profileName   string
	statePath     string
	interfaceName string
	configPath    string
	wakeUser      string
}

func superviseTargets(options SuperviseOptions) []superviseTarget {
	if options.ProfilesDir == "" {
		return []superviseTarget{{
			profileName:   DefaultProfileName,
			statePath:     options.StatePath,
			interfaceName: options.InterfaceName,
			configPath:    options.ConfigPath,
			wakeUser:      options.WakeUser,
		}}
	}

	var targets []superviseTarget
	for _, profile := range AllProfileSettings() {
		if !profile.Supervise || profile.InterfaceName == "" {
			continue
		}
		targets = append(targets, superviseTarget{
			profileName:   profile.ProfileName,
			statePath:     profile.DesiredStatePath(),
			interfaceName: profile.InterfaceName,
			configPath:    profile.TunnelConfigPath(),
			wakeUser:      options.WakeUser,
		})
	}
	return targets
}

// missingConfigLogged keeps the daemon from repeating itself. The condition
// is not transient — a configuration appears when somebody deploys, not on
// its own — so saying it once per occurrence is saying it enough.
var missingConfigLogged = map[string]bool{}

func reconcileTunnel(target superviseTarget) {
	wanted := desiredUp(target.statePath)
	running := tunnel.IsUp(target.interfaceName)

	// A profile whose configuration is not there cannot be raised, and
	// trying every fifteen seconds fills the log with one line that never
	// changes. It happens between deploying a profile and authorising the
	// privileged step, and after a configuration is removed by hand.
	if wanted && !sys.Exists(target.configPath) {
		if !missingConfigLogged[target.profileName] {
			missingConfigLogged[target.profileName] = true
			logf("%s has no configuration at %s; deploy it from Setup. Not retrying until it appears.",
				target.profileName, target.configPath)
		}
		return
	}
	delete(missingConfigLogged, target.profileName)

	switch {
	case wanted && !running:
		logf("bringing %s up", target.interfaceName)
		if err := RaiseTunnel(target.interfaceName, target.configPath); err != nil {
			logf("could not raise %s: %v", target.interfaceName, err)
		}

	case !wanted && running:
		logf("bringing %s down", target.interfaceName)
		if err := tunnel.Down(target.interfaceName); err != nil {
			logf("could not drop %s: %v", target.interfaceName, err)
		}

	case wanted && running && handshakeIsStale(target.interfaceName):
		// The interface exists and the peer has gone quiet. Two things look
		// like this from here: a changed public address, which a re-pin fixes,
		// and a gateway that has switched itself off on its idle timeout,
		// which no amount of re-pinning fixes because there is nothing at the
		// other end to answer.
		//
		// Waking comes first for that reason. It is a no-op on a deployment
		// with no idle timeout, and on one that has it, re-pinning before the
		// gateway is back just burns the next interval.
		wakeGateway(target)

		logf("no handshake on %s for %s; re-pinning", target.interfaceName, staleHandshake)
		if err := tunnel.Down(target.interfaceName); err != nil {
			logf("could not drop %s before re-pinning: %v", target.interfaceName, err)
			return
		}
		if err := RaiseTunnel(target.interfaceName, target.configPath); err != nil {
			logf("re-pin failed: %v", err)
		}
	}
}

// wakeGateway asks AWS to bring the gateway back, as the user who owns the
// deployment.
//
// Best effort, and deliberately quiet about failure: a laptop on a train has
// no route to AWS either, and a daemon that logs a stack trace every fifteen
// seconds is a daemon whose log nobody reads.
func wakeGateway(target superviseTarget) {
	settings, err := LoadProfileSettings(target.profileName)
	if err != nil || settings.IdleTimeoutMinutes == 0 || settings.GatewayGroupName == "" {
		return
	}

	executable := SupervisorExecutable
	if !sys.Exists(executable) {
		if running, err := os.Executable(); err == nil {
			executable = running
		}
	}

	user := target.wakeUser
	if user == "" {
		user = settings.Username
	}
	if user == "" || user == "root" {
		logf("not waking %s: no unprivileged user to read the AWS profile as", target.profileName)
		return
	}

	logf("waking the gateway for %s", target.profileName)
	result := sys.Run("/usr/bin/sudo", "-u", user, executable, "wake",
		"--vpn-profile", target.profileName, "--quiet")
	if !result.OK() {
		logf("could not wake the gateway for %s: %s", target.profileName, strings.TrimSpace(result.Output))
	}
}

// RaiseTunnel reads the config and applies it. Shared with the `tunnel up`
// subcommand, so the supervisor, the menu bar and a person at a terminal do
// the same thing — including the refusal below, which is why it lives here
// rather than in any one of the three.
func RaiseTunnel(interfaceName, configPath string) error {
	if configPath == "" {
		configPath = "/etc/wireguard/" + interfaceName + ".conf"
	}
	file, err := tunnel.Load(configPath)
	if err != nil {
		return err
	}
	if err := CheckLocalNetworks(interfaceName, file); err != nil {
		return err
	}
	return tunnel.Up(tunnel.Options{
		Name:    interfaceName,
		Address: file.Address,
		MTU:     file.MTU,
		Config:  file.Config,
	})
}

// CheckLocalNetworks refuses to raise a tunnel whose routes would take over a
// network this machine is already on.
//
// The tunnel's own interface is excluded from what counts as "already on":
// re-raising a tunnel that is up must not be refused because of the addresses
// it put there itself.
func CheckLocalNetworks(interfaceName string, file tunnel.File) error {
	local, err := tunnel.LocalNetworks(ourDevices(interfaceName))
	local = withoutOurTunnels(local)
	if err != nil {
		// Not fatal. A machine whose interfaces cannot be listed is a machine
		// with bigger problems, and refusing the tunnel would add one.
		logf("could not read this machine's networks, raising %s anyway: %v", interfaceName, err)
		return nil
	}

	collisions := tunnel.Collisions(tunnel.ClaimedBy(file), local)
	if len(collisions) == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s would route a network this machine is already on:\n%s\n\n"+
			"  Raising it would take that network away — the printer, the router, the\n"+
			"  other machines on it. Move to another network, or redeploy this profile\n"+
			"  with a --vpn-cidr that does not overlap.",
		interfaceName, tunnel.Explain(collisions))
}

// ourDevices is every interface this product owns: the one being acted on,
// and the ones the other profiles are using.
//
// A tunnel is not a network to be protected from — least of all from itself.
// Before this, raising a second profile was refused because the first one's
// utun was "a network this machine is already on", which is true and
// useless.
func ourDevices(interfaceName string) []string {
	devices := []string{interfaceName, tunnel.Device(interfaceName)}
	for _, profile := range AllProfileSettings() {
		if profile.InterfaceName == "" {
			continue
		}
		devices = append(devices, profile.InterfaceName, tunnel.Device(profile.InterfaceName))
	}
	return devices
}

// ourAddresses is the second half of recognising our own tunnels, and the
// half that works without privileges.
//
// A running tunnel's utun device can only be matched to a profile by reading
// /var/run/wireguard/<name>.name, which is root-only — so an unprivileged
// deploy could not tell that utun8 carrying 10.110.0.2/32 was the very
// profile being deployed, and refused it for colliding with itself. The
// addresses are in the profile store, which the owner can always read.
func ourAddresses() []string {
	var addresses []string
	for _, profile := range AllProfileSettings() {
		if profile.ClientAddress != "" {
			addresses = append(addresses, profile.ClientAddress)
		}
		if profile.GatewayAddress != "" {
			addresses = append(addresses, profile.GatewayAddress)
		}
	}
	return addresses
}

// withoutOurTunnels drops the interfaces carrying one of this product's own
// tunnel addresses, whatever the kernel happened to call them.
func withoutOurTunnels(local []tunnel.Network, extra ...string) []tunnel.Network {
	ours := map[string]bool{}
	for _, address := range append(ourAddresses(), extra...) {
		if address != "" {
			ours[address] = true
		}
	}

	kept := make([]tunnel.Network, 0, len(local))
	for _, network := range local {
		// A point-to-point tunnel address is a /32, which is what the
		// interface reports once it stops claiming the whole class A.
		size, _ := network.Net.Mask.Size()
		if size == 32 && ours[network.Net.IP.String()] {
			continue
		}
		kept = append(kept, network)
	}
	return kept
}

// handshakeIsStale reports whether every peer has gone quiet. A peer that has
// never handshaken at all counts as stale: the tunnel came up and never
// connected, which is exactly the case worth retrying.
func handshakeIsStale(interfaceName string) bool {
	status, err := tunnel.Report(interfaceName)
	if err != nil {
		// A failure to ask is not evidence of a dead tunnel, and bouncing on it
		// would make a working tunnel flap.
		logf("could not read %s: %v", interfaceName, err)
		return false
	}
	if len(status.Peers) == 0 {
		return false
	}
	return !status.Connected(time.Now())
}

func logf(format string, args ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
}

// --- installing it ---------------------------------------------------------

// SupervisorExecutable is the copy of this binary the daemon runs: the
// root-owned helper, not the app bundle's copy. A daemon that stops working
// because somebody dragged an app to the Trash is worse than no daemon, and a
// root daemon running a user-writable file is worse than both.
const SupervisorExecutable = HelperPath

// InstallSupervisor writes and loads the daemon: a LaunchDaemon on macOS, a
// systemd unit on Linux. Called from the
// privileged stage, which is already root.
func InstallSupervisor(profilesDir string) error {
	if profilesDir == "" {
		return fmt.Errorf("the supervisor needs the path of the profile store")
	}

	executable := SupervisorExecutable
	if !sys.Exists(executable) {
		// A source checkout has no installed helper; use the running one, which
		// is the same build.
		running, err := os.Executable()
		if err != nil {
			return err
		}
		executable = running
	}

	definition := SupervisorDefinition(executable, profilesDir)
	if err := sys.WriteAsRootNonInteractive(definition, SupervisorPath, 0o644); err != nil {
		return err
	}
	return loadSupervisor(true)
}

// RemoveSupervisor is safe to call when it was never installed.
func RemoveSupervisor(asRoot bool) {
	if !sys.Exists(SupervisorPath) {
		return
	}
	unloadSupervisor(asRoot)
	if asRoot {
		os.Remove(SupervisorPath)
	} else {
		sys.RunInteractive("/usr/bin/sudo", "rm", "-f", SupervisorPath)
	}
	ui.Done("Supervisor removed")
}
