// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/sys"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/tunnel"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/ui"
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
//	~/Library/Application Support/XpremVpn/desired-state   "up" or "down"
//
// Root reads that file, compares it to reality, and reconciles. A laptop that
// reboots with the switch on comes back with the tunnel up; one that reboots
// with the switch off comes back with it down.

const (
	SupervisorLabel     = "ca.maragato.xprem.vpn.supervisor"
	SupervisorPlistPath = "/Library/LaunchDaemons/" + SupervisorLabel + ".plist"
	SupervisorLogPath   = "/var/log/xprem-vpn-supervisor.log"

	// A handshake older than this means the far end has stopped answering.
	// PersistentKeepalive is 25s, so three minutes is many missed chances
	// rather than one unlucky moment.
	staleHandshake = 180 * time.Second

	// Long enough that a flapping link is not made worse by bouncing the
	// tunnel on top of it, short enough that a wake from sleep recovers before
	// anyone reaches for the menu bar.
	superviseInterval = 15 * time.Second
)

func DesiredStatePath() string {
	return filepath.Join(AppConfigDir(), "desired-state")
}

// SetDesiredState records what the user last asked for. Written by the menu bar
// and by the installer, both unprivileged.
func SetDesiredState(up bool) error {
	directory := AppConfigDir()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	state := "down"
	if up {
		state = "up"
	}
	return os.WriteFile(DesiredStatePath(), []byte(state+"\n"), 0o644)
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
	StatePath     string
	InterfaceName string
	ConfigPath    string
	Once          bool
}

// Supervise is the body of the LaunchDaemon. It reconciles once per interval
// and never exits, because launchd restarting it is the recovery path for a
// crash rather than something to paper over here.
func Supervise(options SuperviseOptions) {
	logf("supervisor started for %s, watching %s", options.InterfaceName, options.StatePath)

	for {
		reconcileTunnel(options)
		if options.Once {
			return
		}
		time.Sleep(superviseInterval)
	}
}

func reconcileTunnel(options SuperviseOptions) {
	wanted := desiredUp(options.StatePath)
	running := tunnel.IsUp(options.InterfaceName)

	switch {
	case wanted && !running:
		logf("bringing %s up", options.InterfaceName)
		if err := RaiseTunnel(options.InterfaceName, options.ConfigPath); err != nil {
			logf("could not raise %s: %v", options.InterfaceName, err)
		}

	case !wanted && running:
		logf("bringing %s down", options.InterfaceName)
		if err := tunnel.Down(options.InterfaceName); err != nil {
			logf("could not drop %s: %v", options.InterfaceName, err)
		}

	case wanted && running && handshakeIsStale(options.InterfaceName):
		// The interface exists and the peer has gone quiet. This is what a
		// changed public address looks like from here: the tunnel has to be
		// rebuilt for the endpoint to be re-resolved and re-pinned.
		logf("no handshake on %s for %s; re-pinning", options.InterfaceName, staleHandshake)
		if err := tunnel.Down(options.InterfaceName); err != nil {
			logf("could not drop %s before re-pinning: %v", options.InterfaceName, err)
			return
		}
		if err := RaiseTunnel(options.InterfaceName, options.ConfigPath); err != nil {
			logf("re-pin failed: %v", err)
		}
	}
}

// RaiseTunnel reads the config and applies it. Shared with the `tunnel up`
// subcommand, so the supervisor and a person at a terminal do the same thing.
func RaiseTunnel(interfaceName, configPath string) error {
	if configPath == "" {
		configPath = "/etc/wireguard/" + interfaceName + ".conf"
	}
	file, err := tunnel.Load(configPath)
	if err != nil {
		return err
	}
	return tunnel.Up(tunnel.Options{
		Name:    interfaceName,
		Address: file.Address,
		MTU:     file.MTU,
		Config:  file.Config,
	})
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

// SupervisorPlist is the launchd job. The state file's path is baked in as an
// argument because the daemon runs as root and has no way to work out which of
// the machine's users owns the deployment.
func SupervisorPlist(s Settings, executable, statePath string) string {
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
		`    <string>--state</string>`,
		`    <string>` + statePath + `</string>`,
		`    <string>--interface</string>`,
		`    <string>` + s.InterfaceName + `</string>`,
		`    <string>--config</string>`,
		`    <string>` + s.TunnelConfigPath() + `</string>`,
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
func InstallSupervisor(s Settings, statePath string) error {
	if statePath == "" {
		return fmt.Errorf("the supervisor needs the path of the desired-state file")
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

	plist := SupervisorPlist(s, executable, statePath)
	if err := sys.WriteAsRootNonInteractive(plist, SupervisorPlistPath, 0o644); err != nil {
		return err
	}

	// Unloaded first: launchd keeps running the old job definition otherwise,
	// so a changed interface name or wg-quick path would not take effect until
	// the next reboot.
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
