// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

// Linux is headless here: no menu bar app, so the tunnel is moved with
// `mymicrotunnel tunnel up|down` and held across reboots by a systemd unit
// rather than a LaunchDaemon. Everything else — the deploy, the sudoers rule,
// /etc/wireguard — is the same as on macOS.

// HasApp is whether this platform has the menu bar app.
const HasApp = false

const (
	// HelperPath is the copy the sudoers rule names. The same argument as on
	// macOS applies: a NOPASSWD rule is only as trustworthy as the file it
	// points at. /usr/libexec is root:root 0755 on Debian and belongs to no
	// user-writable package manager, unlike /usr/local, which older Debian
	// releases made group-writable by staff.
	HelperPath = "/usr/libexec/mymicrotunnel/mymicrotunnel"

	SupervisorUnit = "mymicrotunnel-supervisor.service"
	SupervisorPath = "/etc/systemd/system/" + SupervisorUnit
)

// AppConfigDir is under ~/.config, fixed rather than read from
// XDG_CONFIG_HOME: sudo and systemd do not carry the user's environment, and
// the root stage has to find the same directory the user's stage wrote.
func AppConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mymicrotunnel")
}

// SupervisorDefinition is the systemd unit. The profile store's path is baked
// in for the same reason as the plist's: the daemon is root and cannot work
// out which user owns the deployments.
func SupervisorDefinition(executable, profilesDir string) string {
	return strings.Join([]string{
		"# Installed by MyMicroTunnel. Holds each supervised tunnel at the state",
		"# its owner last asked for, across reboots.",
		"[Unit]",
		"Description=MyMicroTunnel supervisor",
		"Wants=network-online.target",
		"After=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		fmt.Sprintf("ExecStart=%q supervise --profiles %q --wake-user %q",
			executable, profilesDir, CurrentUsername()),
		"Restart=always",
		"RestartSec=5s",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

// loadSupervisor enables the unit and restarts it, so a changed definition
// takes effect now rather than at the next boot.
func loadSupervisor(asRoot bool) error {
	steps := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", SupervisorUnit},
		{"systemctl", "restart", SupervisorUnit},
	}
	for _, step := range steps {
		if asRoot {
			if result := sys.Run(step[0], step[1:]...); !result.OK() {
				return fmt.Errorf("%s: %s", strings.Join(step, " "), result.Output)
			}
			continue
		}
		if sys.RunInteractive("/usr/bin/sudo", step...) != 0 {
			return fmt.Errorf("%s failed", strings.Join(step, " "))
		}
	}
	return nil
}

func unloadSupervisor(asRoot bool) {
	if asRoot {
		sys.Run("systemctl", "disable", "--now", SupervisorUnit)
		return
	}
	sys.RunInteractive("/usr/bin/sudo", "systemctl", "disable", "--now", SupervisorUnit)
}

func supervisorState() string {
	return strings.TrimSpace(sys.Run("systemctl", "is-active", SupervisorUnit).Output)
}

func supervisorLog(lines int) string {
	if log := sys.Run("journalctl", "-u", SupervisorUnit, "-n", strconv.Itoa(lines),
		"--no-pager", "-o", "cat"); log.OK() {
		return log.Output
	}
	return ""
}

func operatingSystem() (string, string) {
	content, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "OS", ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		if value, found := strings.CutPrefix(line, "PRETTY_NAME="); found {
			return "OS", strings.Trim(value, `"`)
		}
	}
	return "OS", ""
}

func installedPaths() []string { return []string{CommandPath, HelperPath} }

func appVersionOf(string) string { return "" }

// consoleUser is who asked for root when it was not sudo: pkexec records the
// caller's uid.
func consoleUser() string {
	if uid := os.Getenv("PKEXEC_UID"); uid != "" {
		if found, err := user.LookupId(uid); err == nil {
			return found.Username
		}
	}
	return ""
}

// pingArgs is two pings, five seconds each. Linux spells the timeout -W.
func pingArgs(address string) []string {
	return []string{"-c", "2", "-W", "5", address}
}

// OpenApp has nothing to open on Linux.
func OpenApp() {}

func removeApp() {}

// Prerequisites checks for the kernel module and the two tools the Linux
// tunnel drives.
func Prerequisites(interactive bool) string {
	ui.Step("Checking prerequisites")

	if missing := tunnel.Tools(); len(missing) > 0 {
		ui.Fail("Missing %s. On Debian: sudo apt install wireguard-tools iproute2",
			strings.Join(missing, " and "))
	}
	// Loaded on demand by `ip link add … type wireguard`, so its absence from
	// /sys/module is only a problem when modinfo cannot find it either.
	if !sys.Exists("/sys/module/wireguard") && !sys.Run("modinfo", "wireguard").OK() {
		ui.Fail("The wireguard kernel module is not available. Debian's stock kernel has it; " +
			"a container or a custom kernel may not.")
	}
	ui.Done("Kernel WireGuard, wg and ip present")
	return ""
}

// InstallApp has no app to install on Linux.
func InstallApp(string) {
	ui.Step("Menu bar app")
	ui.Done("None on Linux; the tunnel is moved with `%s tunnel up|down`", CommandPath)
}

// RegisterLoginItem has nothing to register on Linux: the supervisor is what
// restores the tunnel at boot.
func RegisterLoginItem() {
	ui.Done("Not applicable on Linux; install with --supervise to restore the tunnel at boot")
}
