// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"os"
	"strings"
	"testing"
)

func acceptSupervisor(fake *fakeMachine) { fake.reply("systemctl", "") }

func answerSupervisorState(fake *fakeMachine, state, log string) {
	fake.reply("systemctl is-active", state+"\n")
	fake.reply("journalctl -u "+SupervisorUnit+" -n 12", log)
}

// answerSupervisorUnloaded makes systemd report the unit stopped, and returns how the
// report says so: systemctl answers "inactive" rather than nothing.
func answerSupervisorUnloaded(fake *fakeMachine) string {
	fake.refuse("systemctl is-active", "inactive\n")
	return "state                  inactive"
}

func stubTools(t *testing.T, missing ...string) {
	t.Helper()
	original := tunnelTools
	t.Cleanup(func() { tunnelTools = original })
	tunnelTools = func() []string { return missing }
}

// A changed unit takes effect now rather than at the next boot.
func TestLoadingTheSupervisorReloadsEnablesAndRestarts(t *testing.T) {
	fake := stubMachine(t)
	acceptSupervisor(fake)

	if err := loadSupervisor(true); err != nil {
		t.Fatalf("loading as root: %v", err)
	}
	want := []string{
		"systemctl daemon-reload",
		"systemctl enable " + SupervisorUnit,
		"systemctl restart " + SupervisorUnit,
	}
	if strings.Join(fake.commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran %v, want %v", fake.commands, want)
	}
}

func TestARefusedUnitSaysWhichStepAndWhy(t *testing.T) {
	fake := stubMachine(t)
	acceptSupervisor(fake)
	fake.refuse("systemctl enable", "Unit file is masked.")

	err := loadSupervisor(true)
	if err == nil || err.Error() != "systemctl enable "+SupervisorUnit+": Unit file is masked." {
		t.Errorf("the refusal was not passed on: %v", err)
	}
	if fake.ran("systemctl restart") {
		t.Errorf("went on after a refusal: %v", fake.commands)
	}
}

func TestLoadingTheUnitUnprivilegedGoesThroughSudo(t *testing.T) {
	fake := stubMachine(t)

	if err := loadSupervisor(false); err != nil {
		t.Fatalf("loading through sudo: %v", err)
	}
	if !fake.ran("/usr/bin/sudo systemctl restart " + SupervisorUnit) {
		t.Errorf("not restarted through sudo: %v", fake.commands)
	}

	fake.exits["/usr/bin/sudo systemctl daemon-reload"] = 1
	if err := loadSupervisor(false); err == nil || err.Error() != "systemctl daemon-reload failed" {
		t.Errorf("a refused reload was reported as %v", err)
	}
}

func TestUnloadingTheUnitDisablesItNow(t *testing.T) {
	fake := stubMachine(t)
	unloadSupervisor(true)
	unloadSupervisor(false)
	want := []string{
		"systemctl disable --now " + SupervisorUnit,
		"/usr/bin/sudo systemctl disable --now " + SupervisorUnit,
	}
	if strings.Join(fake.commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran %v, want %v", fake.commands, want)
	}
}

func TestTheUnitStateAndLogComeFromSystemd(t *testing.T) {
	fake := stubMachine(t)
	if log := supervisorLog(5); log != "" {
		t.Errorf("an unreadable journal is %q", log)
	}
	answerSupervisorState(fake, "active", "one\ntwo")
	if state := supervisorState(); state != "active" {
		t.Errorf("state is %q", state)
	}
	if log := supervisorLog(12); log != "one\ntwo" {
		t.Errorf("log is %q", log)
	}
}

func TestTheOperatingSystemIsThePrettyName(t *testing.T) {
	fake := stubMachine(t)
	if name, release := operatingSystem(); name != "OS" || release != "" {
		t.Errorf("without os-release: %q %q", name, release)
	}

	fake.place(t, "/etc/os-release", "NAME=Debian\nID=debian\n", 0o644)
	if _, release := operatingSystem(); release != "" {
		t.Errorf("a file with no PRETTY_NAME gave %q", release)
	}

	fake.place(t, "/etc/os-release", "NAME=Debian\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n", 0o644)
	if name, release := operatingSystem(); name != "OS" || release != "Debian GNU/Linux 13 (trixie)" {
		t.Errorf("got %q %q", name, release)
	}
}

// pkexec records the caller's uid, and that is who the sudoers rule names.
func TestARootProcessStartedByPkexecNamesItsCaller(t *testing.T) {
	stubMachine(t)
	t.Setenv("PKEXEC_UID", "")
	if name := consoleUser(); name != "" {
		t.Errorf("with no caller recorded, the console user is %q", name)
	}
	t.Setenv("PKEXEC_UID", "0")
	if name := consoleUser(); name != "root" {
		t.Errorf("uid 0 is %q", name)
	}
	t.Setenv("PKEXEC_UID", "999999")
	if name := consoleUser(); name != "" {
		t.Errorf("an unknown uid is %q", name)
	}
}

func TestLinuxSpellsThePingTimeoutDashW(t *testing.T) {
	if args := strings.Join(pingArgs("10.100.0.1"), " "); args != "-c 2 -W 5 10.100.0.1" {
		t.Errorf("ping %s", args)
	}
}

func TestLinuxHasNoAppToOpenInstallOrRemove(t *testing.T) {
	fake := stubMachine(t)

	output := captured(t, func() {
		OpenApp()
		removeApp()
		InstallApp("/src/checkout")
		RegisterLoginItem()
	})

	if len(fake.commands) != 0 {
		t.Errorf("ran %v", fake.commands)
	}
	if !strings.Contains(output, "None on Linux; the tunnel is moved with `"+CommandPath+" tunnel up|down`") ||
		!strings.Contains(output, "install with --supervise to restore the tunnel at boot") {
		t.Errorf("not explained:\n%s", output)
	}
	if appVersionOf("/anything") != "" {
		t.Error("Linux reported an app version")
	}
	if paths := strings.Join(installedPaths(), " "); paths != CommandPath+" "+HelperPath {
		t.Errorf("installed paths %s", paths)
	}
}

func TestPrerequisitesNameWhatToInstall(t *testing.T) {
	stubMachine(t)
	stubTools(t, "wg", "ip")

	var message string
	captured(t, func() { message = failureOf(func() { Prerequisites(true) }) })

	if message != "Missing wg and ip. On Debian: sudo apt install wireguard-tools iproute2" {
		t.Errorf("failed with %q", message)
	}
}

// The module is loaded on demand, so its absence from /sys/module is only a
// problem when modinfo cannot find it either.
func TestPrerequisitesAcceptAModuleThatLoadsOnDemand(t *testing.T) {
	fake := stubMachine(t)
	stubTools(t)

	var message string
	captured(t, func() { message = failureOf(func() { Prerequisites(true) }) })
	if !strings.HasPrefix(message, "The wireguard kernel module is not available") {
		t.Errorf("failed with %q", message)
	}

	fake.reply("modinfo wireguard", "filename: wireguard.ko")
	var engine string
	output := captured(t, func() { engine = Prerequisites(true) })
	if engine != "" || !strings.Contains(output, "Kernel WireGuard, wg and ip present") {
		t.Errorf("engine %q, output:\n%s", engine, output)
	}

	delete(fake.replies, "modinfo wireguard")
	if err := os.MkdirAll(onDisk("/sys/module/wireguard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if message := failureOf(func() { captured(t, func() { Prerequisites(true) }) }); message != "" {
		t.Errorf("a loaded module was refused: %q", message)
	}
}
