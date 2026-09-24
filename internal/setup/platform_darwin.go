// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

// What only macOS has: the app bundle, launchd, and the layout Apple documents
// for privileged helpers. platform_linux.go is the same surface for Linux.

// HasApp is whether this platform has the menu bar app.
const HasApp = true

const (
	// HelperPath is the copy the sudoers rule names, and it is deliberately not
	// the one above.
	//
	// A NOPASSWD rule is only as trustworthy as the file it points at. The
	// first version of this product pointed at /opt/homebrew/bin/wg-quick,
	// which Homebrew installs into a directory owned by the user and group
	// admin, mode 775 — so the very user the rule names could replace that file
	// and become root without a password. Homebrew on Intel does the same to
	// /usr/local. /Library/PrivilegedHelperTools is root:wheel, is the location
	// Apple documents for exactly this, and is not somewhere a package manager
	// takes ownership of.
	HelperPath = "/Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper"

	SupervisorLabel   = "ca.maragato.mymicrotunnel.supervisor"
	SupervisorPath    = "/Library/LaunchDaemons/" + SupervisorLabel + ".plist"
	SupervisorLogPath = "/var/log/mymicrotunnel-supervisor.log"
)

func AppConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MyMicroTunnel")
}

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

// SupervisorDefinition is what is written to SupervisorPath.
func SupervisorDefinition(executable, profilesDir string) string {
	return SupervisorPlist(executable, profilesDir)
}

// loadSupervisor (re)loads the job. Unloaded first: launchd keeps running the
// old job definition otherwise, so a changed profile store or a new argument
// list would not take effect until the next reboot.
func loadSupervisor(asRoot bool) error {
	if asRoot {
		sys.Run("/bin/launchctl", "bootout", "system/"+SupervisorLabel)
		if result := sys.Run("/bin/launchctl", "bootstrap", "system", SupervisorPath); !result.OK() {
			return fmt.Errorf("launchctl refused the supervisor: %s", result.Output)
		}
		return nil
	}
	sys.RunInteractive("/usr/bin/sudo", "/bin/launchctl", "bootout", "system/"+SupervisorLabel)
	if sys.RunInteractive("/usr/bin/sudo", "/bin/launchctl", "bootstrap", "system", SupervisorPath) != 0 {
		return fmt.Errorf("launchctl refused the supervisor")
	}
	return nil
}

func unloadSupervisor(asRoot bool) {
	if asRoot {
		sys.Run("/bin/launchctl", "bootout", "system/"+SupervisorLabel)
		return
	}
	sys.RunInteractive("/usr/bin/sudo", "/bin/launchctl", "bootout", "system/"+SupervisorLabel)
}

// supervisorState is launchd's one-line view of the job, or "" when it is not
// loaded.
func supervisorState() string {
	state := sys.Run("/bin/launchctl", "print", "system/"+SupervisorLabel)
	if !state.OK() {
		return ""
	}
	for _, line := range strings.Split(state.Output, "\n") {
		if strings.Contains(line, "state =") {
			return strings.TrimSpace(line)
		}
	}
	return "loaded"
}

func supervisorLog(lines int) string {
	if log := sys.Run("/usr/bin/tail", "-n", fmt.Sprint(lines), SupervisorLogPath); log.OK() {
		return log.Output
	}
	return ""
}

func operatingSystem() (string, string) {
	if product := sys.Run("/usr/bin/sw_vers", "-productVersion"); product.OK() {
		return "macOS", product.Output
	}
	return "macOS", ""
}

func installedPaths() []string {
	return []string{InstalledAppPath, CommandPath, HelperPath, EmbeddedEngineDir + "/wireguard-go"}
}

func appVersionOf(bundle string) string {
	output, err := exec.Command("/usr/libexec/PlistBuddy",
		"-c", "Print :CFBundleShortVersionString", bundle+"/Contents/Info.plist").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// consoleUser is who owns the login session, for a root process that was not
// started through sudo.
func consoleUser() string {
	if console := sys.Run("/usr/bin/stat", "-f%Su", "/dev/console"); console.OK() {
		return strings.TrimSpace(console.Output)
	}
	return ""
}

// pingArgs is two pings, five seconds each. macOS spells the timeout -t.
func pingArgs(address string) []string {
	return []string{"-c", "2", "-t", "5", address}
}

// OpenApp launches the menu bar app.
func OpenApp() { sys.Run("/usr/bin/open", InstalledAppPath) }

func removeApp() {
	sys.Run("/usr/bin/pkill", "-f", "MyMicroTunnel.app/Contents/MacOS/MyMicroTunnel")
	sys.Run("osascript", "-e",
		`tell application "System Events" to delete (every login item whose name is "MyMicroTunnel")`)
	os.RemoveAll(InstalledAppPath)
	ui.Done("App and login item removed")
}

// Prerequisites checks that the one native thing this product needs is present.
//
// It used to install wireguard-tools through Homebrew, which meant a customer
// without Homebrew had to run an installer before running the installer, and a
// customer with it ended up trusting a root-capable binary in a directory their
// own account could write to. The package now carries wireguard-go itself, so
// there is nothing to fetch and nothing to trust that did not arrive signed.
func Prerequisites(interactive bool) string {
	ui.Step("Checking prerequisites")

	engine, err := tunnel.Engine("")
	if err != nil {
		ui.Fail("%v", err)
	}
	ui.Done("wireguard-go at %s", engine)
	return engine
}

// InstallApp is a no-op when the package already placed the app; it only builds
// when running from a source checkout.
func InstallApp(repoRoot string) {
	// An empty root means this is the installed copy rather than one running
	// from a checkout. Joining "" with "menubar" produces a *relative* path,
	// which resolves against whatever directory the command happened to be run
	// from — so running /usr/local/bin/mymicrotunnel while sitting in a
	// checkout made it try to rebuild the app and copy it over the signed
	// bundle the package had just installed.
	menubarDir := ""
	if repoRoot != "" {
		menubarDir = filepath.Join(repoRoot, "menubar")
	}
	if menubarDir == "" || !sys.Exists(menubarDir) {
		ui.Step("Menu bar app")
		if sys.Exists(InstalledAppPath) {
			ui.Done("Already installed at %s", InstalledAppPath)
			return
		}
		// Running from inside a bundle that is not in /Applications yet: the
		// app can install itself rather than declaring the situation hopeless.
		if bundle := enclosingBundle(); bundle != "" {
			if result := sys.Run("cp", "-R", bundle, "/Applications/"); result.OK() {
				ui.Done("Copied %s to %s", bundle, InstalledAppPath)
				return
			}
		}
		ui.Fail("%s is missing and there are no sources to build it from.", InstalledAppPath)
	}

	// Never as root. The privileged stage runs from the same checkout, and a
	// build it performs leaves root-owned objects under menubar/build that
	// the developer who owns the tree cannot delete or overwrite — every
	// later build then fails on "File exists" from lipo, with nothing saying
	// why. Observed exactly that way.
	if os.Geteuid() == 0 {
		if sys.Exists(filepath.Join(menubarDir, "build", "MyMicroTunnel.app")) {
			ui.Step("Menu bar app")
			if result := sys.Run("cp", "-R",
				filepath.Join(menubarDir, "build", "MyMicroTunnel.app"), "/Applications/"); result.OK() {
				ui.Done("%s", InstalledAppPath)
				return
			}
		}
		ui.Warn("Not building the app as root; run `make -C menubar install` as yourself.")
		return
	}

	ui.Step("Building the menu bar app")
	if sys.RunInteractive("make", "-C", menubarDir, "app") != 0 {
		ui.Fail("The app did not build.")
	}

	sys.Run("/usr/bin/pkill", "-f", "MyMicroTunnel.app/Contents/MacOS/MyMicroTunnel")
	os.RemoveAll(InstalledAppPath)
	if result := sys.Run("cp", "-R", filepath.Join(menubarDir, "build", "MyMicroTunnel.app"), "/Applications/"); !result.OK() {
		ui.Fail("Could not copy the app into /Applications: %s", result.Output)
	}
	ui.Done("%s", InstalledAppPath)
}

func RegisterLoginItem() {
	// Opening at login only puts the switch in the menu bar; it does not raise
	// the tunnel, which stays a deliberate act. When the supervisor is
	// installed, the tunnel's state at boot comes from the desired-state file
	// instead, which is the user's own last decision rather than a default.
	sys.Run("osascript", "-e",
		`tell application "System Events" to delete (every login item whose name is "MyMicroTunnel")`)
	result := sys.Run("osascript", "-e",
		`tell application "System Events" to make login item at end with properties {path:"`+InstalledAppPath+`", hidden:true}`)
	if !result.OK() {
		ui.Warn("Could not register the login item: %s", result.Output)
		ui.Info("Add it under System Settings › General › Login Items.")
		return
	}
	ui.Done("Registered")
}

// enclosingBundle returns the .app this binary is running inside, or "" when it
// is a plain command-line build.
func enclosingBundle() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	directory := filepath.Dir(executable)
	for attempt := 0; attempt < 4; attempt++ {
		if strings.HasSuffix(directory, ".app") {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return ""
}
