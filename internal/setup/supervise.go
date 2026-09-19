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
	SupervisorLabel     = "ca.maragato.mymicrotunnel.supervisor"
	SupervisorPlistPath = "/Library/LaunchDaemons/" + SupervisorLabel + ".plist"
	SupervisorLogPath   = "/var/log/mymicrotunnel-supervisor.log"

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

// Supervise is the body of the LaunchDaemon. It reconciles once per interval
// and never exits, because launchd restarting it is the recovery path for a
// crash rather than something to paper over here.
func Supervise(options SuperviseOptions) {
	if options.ProfilesDir != "" {
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

func reconcileTunnel(target superviseTarget) {
	wanted := desiredUp(target.statePath)
	running := tunnel.IsUp(target.interfaceName)

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
	local, err := tunnel.LocalNetworks([]string{interfaceName, tunnel.Device(interfaceName)})
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

// SupervisorPlist is the launchd job. The profile store's path is baked in as
// an argument because the daemon runs as root and has no way to work out which
// of the machine's users owns the deployments.
func SupervisorPlist(executable, profilesDir string) string {
	return strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key>`,
		`  <string>` + SupervisorLabel + `</string>`,
		`  <key>ProgramArguments</key>`,
		`  <array>`,
		`    <string>` + executable + `</string>`,
		`    <string>supervise</string>`,
		`    <string>--profiles</string>`,
		`    <string>` + profilesDir + `</string>`,
		`    <string>--wake-user</string>`,
		`    <string>` + CurrentUsername() + `</string>`,
		`  </array>`,
		`  <key>RunAtLoad</key>`,
		`  <true/>`,
		`  <key>KeepAlive</key>`,
		`  <true/>`,
		`  <key>StandardOutPath</key>`,
		`  <string>` + SupervisorLogPath + `</string>`,
		`  <key>StandardErrorPath</key>`,
		`  <string>` + SupervisorLogPath + `</string>`,
		`</dict>`,
		`</plist>`,
		``,
	}, "\n")
}

// SupervisorExecutable is the copy of this binary the daemon runs: the
// root-owned helper, not the app bundle's copy. A daemon that stops working
// because somebody dragged an app to the Trash is worse than no daemon, and a
// root daemon running a user-writable file is worse than both.
const SupervisorExecutable = HelperPath

// InstallSupervisor writes and loads the LaunchDaemon. Called from the
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

	plist := SupervisorPlist(executable, profilesDir)
	if err := sys.WriteAsRootNonInteractive(plist, SupervisorPlistPath, 0o644); err != nil {
		return err
	}

	// Unloaded first: launchd keeps running the old job definition otherwise,
	// so a changed profile store or a new argument list would not take effect
	// until the next reboot.
	sys.Run("/bin/launchctl", "bootout", "system/"+SupervisorLabel)
	if result := sys.Run("/bin/launchctl", "bootstrap", "system", SupervisorPlistPath); !result.OK() {
		return fmt.Errorf("launchctl refused the supervisor: %s", result.Output)
	}
	return nil
}

// RemoveSupervisor is safe to call when it was never installed.
func RemoveSupervisor(asRoot bool) {
	if !sys.Exists(SupervisorPlistPath) {
		return
	}
	if asRoot {
		sys.Run("/bin/launchctl", "bootout", "system/"+SupervisorLabel)
		os.Remove(SupervisorPlistPath)
	} else {
		sys.RunInteractive("/usr/bin/sudo", "/bin/launchctl", "bootout", "system/"+SupervisorLabel)
		sys.RunInteractive("/usr/bin/sudo", "rm", "-f", SupervisorPlistPath)
	}
	ui.Done("Supervisor removed")
}
