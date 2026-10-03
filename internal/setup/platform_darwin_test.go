// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func acceptSupervisor(fake *fakeMachine) { fake.reply("/bin/launchctl", "") }

func answerSupervisorState(fake *fakeMachine, state, log string) {
	fake.reply("/bin/launchctl print", "state = "+state)
	fake.reply("/usr/bin/tail -n 12 "+SupervisorLogPath, log)
}

// answerSupervisorUnloaded makes launchd not know the job, and returns how the
// report says so.
func answerSupervisorUnloaded(fake *fakeMachine) string {
	fake.refuse("/bin/launchctl print", "Could not find service")
	return "loaded                 no"
}

// launchd keeps running the old definition of a job that is bootstrapped over
// itself, so a changed store would not take effect until the next reboot.
func TestLoadingTheSupervisorBootsOutTheOldJobFirst(t *testing.T) {
	fake := stubMachine(t)
	acceptSupervisor(fake)

	if err := loadSupervisor(true); err != nil {
		t.Fatalf("loading as root: %v", err)
	}
	want := []string{
		"/bin/launchctl bootout system/" + SupervisorLabel,
		"/bin/launchctl bootstrap system " + SupervisorPath,
	}
	if strings.Join(fake.commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran %v, want %v", fake.commands, want)
	}
}

func TestARefusedSupervisorSaysWhatLaunchctlSaid(t *testing.T) {
	fake := stubMachine(t)
	fake.refuse("/bin/launchctl bootstrap", "Bootstrap failed: 5: Input/output error")

	err := loadSupervisor(true)
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("the refusal was not passed on: %v", err)
	}
}

func TestLoadingTheSupervisorUnprivilegedGoesThroughSudo(t *testing.T) {
	fake := stubMachine(t)

	if err := loadSupervisor(false); err != nil {
		t.Fatalf("loading through sudo: %v", err)
	}
	if !fake.ran("/usr/bin/sudo /bin/launchctl bootstrap system " + SupervisorPath) {
		t.Errorf("the job was not bootstrapped through sudo: %v", fake.commands)
	}

	fake.exits["/usr/bin/sudo /bin/launchctl bootstrap"] = 1
	if err := loadSupervisor(false); err == nil {
		t.Error("a refused bootstrap was reported as loaded")
	}
}

func TestUnloadingTheSupervisor(t *testing.T) {
	fake := stubMachine(t)
	unloadSupervisor(true)
	unloadSupervisor(false)
	want := []string{
		"/bin/launchctl bootout system/" + SupervisorLabel,
		"/usr/bin/sudo /bin/launchctl bootout system/" + SupervisorLabel,
	}
	if strings.Join(fake.commands, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran %v, want %v", fake.commands, want)
	}
}

func TestTheSupervisorStateIsLaunchdsOwnLine(t *testing.T) {
	fake := stubMachine(t)
	if state := supervisorState(); state != "" {
		t.Errorf("a job launchd does not know is %q, want empty", state)
	}

	fake.reply("/bin/launchctl print", "system/"+SupervisorLabel+" = {\n\tactive count = 1\n\tstate = running\n}")
	if state := supervisorState(); state != "state = running" {
		t.Errorf("state is %q", state)
	}

	fake.reply("/bin/launchctl print", "system/"+SupervisorLabel+" = {\n}")
	if state := supervisorState(); state != "loaded" {
		t.Errorf("a job with no state line is %q, want loaded", state)
	}
}

func TestTheSupervisorLogIsItsTail(t *testing.T) {
	fake := stubMachine(t)
	if log := supervisorLog(3); log != "" {
		t.Errorf("an unreadable log is %q", log)
	}
	fake.reply("/usr/bin/tail -n 3 "+SupervisorLogPath, "one\ntwo\nthree")
	if log := supervisorLog(3); log != "one\ntwo\nthree" {
		t.Errorf("log is %q", log)
	}
}

func TestTheOperatingSystemIsTheProductVersion(t *testing.T) {
	fake := stubMachine(t)
	if name, release := operatingSystem(); name != "macOS" || release != "" {
		t.Errorf("without sw_vers: %q %q", name, release)
	}
	fake.reply("/usr/bin/sw_vers -productVersion", "26.1")
	if name, release := operatingSystem(); name != "macOS" || release != "26.1" {
		t.Errorf("got %q %q", name, release)
	}
}

func TestTheAppVersionIsReadFromTheBundle(t *testing.T) {
	if _, err := os.Stat("/usr/libexec/PlistBuddy"); err != nil {
		t.Skip("no PlistBuddy on this machine")
	}
	bundle := filepath.Join(t.TempDir(), "MyMicroTunnel.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>1.4.2</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}

	if version := appVersionOf(bundle); version != "1.4.2" {
		t.Errorf("version is %q, want 1.4.2", version)
	}
	if version := appVersionOf(filepath.Join(t.TempDir(), "Missing.app")); version != "" {
		t.Errorf("a missing bundle has version %q", version)
	}
}

func TestTheInstalledPathsIncludeTheAppAndTheEngine(t *testing.T) {
	paths := strings.Join(installedPaths(), "\n")
	for _, expected := range []string{InstalledAppPath, CommandPath, HelperPath, EmbeddedEngineDir + "/wireguard-go"} {
		if !strings.Contains(paths, expected) {
			t.Errorf("%s is not checked", expected)
		}
	}
}

func TestMacOSSpellsThePingTimeoutDashT(t *testing.T) {
	if args := strings.Join(pingArgs("10.100.0.1"), " "); args != "-c 2 -t 5 10.100.0.1" {
		t.Errorf("ping %s", args)
	}
}

// Under `osascript … with administrator privileges` there is no SUDO_USER, and
// naming root in the sudoers rule grants the actual user nothing.
func TestARootProcessWithoutSudoNamesTheConsoleUser(t *testing.T) {
	fake := stubMachine(t)
	fake.euid = 0
	fake.reply("/usr/bin/stat -f%Su /dev/console", "alice\n")

	if name := CurrentUsername(); name != "alice" {
		t.Errorf("the rule would name %q, want alice", name)
	}
}

func TestOpeningTheApp(t *testing.T) {
	fake := stubMachine(t)
	OpenApp()
	if !fake.ran("/usr/bin/open " + InstalledAppPath) {
		t.Errorf("ran %v", fake.commands)
	}
}

func TestRemovingTheAppTakesTheLoginItemWithIt(t *testing.T) {
	fake := stubMachine(t)
	fake.place(t, InstalledAppPath+"/Contents/Info.plist", "", 0o644)

	output := captured(t, removeApp)

	if _, err := os.Stat(onDisk(InstalledAppPath)); !os.IsNotExist(err) {
		t.Errorf("the app is still installed: %v", err)
	}
	if !fake.ran("/usr/bin/pkill -f MyMicroTunnel.app") || !fake.ran(`osascript -e tell application "System Events" to delete`) {
		t.Errorf("the running app or its login item was left: %v", fake.commands)
	}
	if !strings.Contains(output, "App and login item removed") {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestPrerequisitesFindTheBundledEngine(t *testing.T) {
	fake := stubMachine(t)
	fake.engine = "/usr/local/lib/mymicrotunnel/wireguard-go"

	var engine string
	output := captured(t, func() { engine = Prerequisites(true) })

	if engine != fake.engine {
		t.Errorf("engine is %q", engine)
	}
	if !strings.Contains(output, "wireguard-go at "+fake.engine) {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestPrerequisitesStopWithoutAnEngine(t *testing.T) {
	fake := stubMachine(t)
	fake.engineErr = errors.New("wireguard-go is missing")

	var message string
	captured(t, func() { message = failureOf(func() { Prerequisites(true) }) })

	if message != "wireguard-go is missing" {
		t.Errorf("failed with %q", message)
	}
}

// --- the app -----------------------------------------------------------------

func TestAnInstalledAppIsLeftWhereThePackagePutIt(t *testing.T) {
	fake := stubMachine(t)
	fake.place(t, InstalledAppPath+"/Contents/Info.plist", "", 0o644)

	output := captured(t, func() { InstallApp("") })

	if fake.ran("cp") || fake.ran("make") {
		t.Errorf("the signed app was replaced: %v", fake.commands)
	}
	if !strings.Contains(output, "Already installed at "+InstalledAppPath) {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestAMissingAppWithNoSourcesStopsTheInstall(t *testing.T) {
	stubMachine(t)

	var message string
	captured(t, func() { message = failureOf(func() { InstallApp("") }) })

	if !strings.Contains(message, "is missing and there are no sources to build it from") {
		t.Errorf("failed with %q", message)
	}
}

// A build run as root leaves objects under menubar/build that the developer
// who owns the tree cannot overwrite, and every later build fails.
func TestRootNeverBuildsTheApp(t *testing.T) {
	fake := stubMachine(t)
	fake.euid = 0
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "menubar"), 0o755); err != nil {
		t.Fatal(err)
	}

	output := captured(t, func() { InstallApp(checkout) })

	if fake.ran("make") {
		t.Errorf("root built the app: %v", fake.commands)
	}
	if !strings.Contains(output, "Not building the app as root") {
		t.Errorf("not explained:\n%s", output)
	}
}

func TestRootCopiesAnAppTheDeveloperAlreadyBuilt(t *testing.T) {
	fake := stubMachine(t)
	fake.euid = 0
	checkout := t.TempDir()
	built := filepath.Join(checkout, "menubar", "build", "MyMicroTunnel.app")
	if err := os.MkdirAll(built, 0o755); err != nil {
		t.Fatal(err)
	}
	fake.reply("cp -R "+built, "")

	captured(t, func() { InstallApp(checkout) })

	if !fake.ran("cp -R " + built + " /Applications/") {
		t.Errorf("the built app was not copied: %v", fake.commands)
	}
	if fake.ran("make") {
		t.Errorf("root built the app: %v", fake.commands)
	}
}

func TestACheckoutBuildsAndInstallsTheApp(t *testing.T) {
	fake := stubMachine(t)
	checkout := t.TempDir()
	menubar := filepath.Join(checkout, "menubar")
	if err := os.MkdirAll(menubar, 0o755); err != nil {
		t.Fatal(err)
	}
	fake.place(t, InstalledAppPath+"/old", "", 0o644)
	fake.reply("cp -R", "")

	output := captured(t, func() { InstallApp(checkout) })

	if !fake.ran("make -C " + menubar + " app") {
		t.Errorf("the app was not built: %v", fake.commands)
	}
	if _, err := os.Stat(onDisk(InstalledAppPath + "/old")); !os.IsNotExist(err) {
		t.Errorf("the old app was not removed before the copy: %v", err)
	}
	if !strings.Contains(output, "✓ "+InstalledAppPath) {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestAnAppThatDoesNotBuildStopsTheInstall(t *testing.T) {
	fake := stubMachine(t)
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "menubar"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake.exits["make"] = 2

	var message string
	captured(t, func() { message = failureOf(func() { InstallApp(checkout) }) })
	if message != "The app did not build." {
		t.Errorf("failed with %q", message)
	}

	fake.exits["make"] = 0
	fake.refuse("cp -R", "Permission denied")
	captured(t, func() { message = failureOf(func() { InstallApp(checkout) }) })
	if message != "Could not copy the app into /Applications: Permission denied" {
		t.Errorf("failed with %q", message)
	}
}

func TestTheLoginItemIsReplacedNotDuplicated(t *testing.T) {
	fake := stubMachine(t)
	fake.reply("osascript", "")

	output := captured(t, RegisterLoginItem)

	if len(fake.commands) != 2 || !strings.Contains(fake.commands[0], "delete") ||
		!strings.Contains(fake.commands[1], `make login item at end with properties {path:"`+InstalledAppPath+`"`) {
		t.Errorf("ran %v", fake.commands)
	}
	if !strings.Contains(output, "Registered") {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestALoginItemThatCannotBeRegisteredSaysWhereToAddIt(t *testing.T) {
	fake := stubMachine(t)
	fake.refuse("osascript", "not authorised")

	output := captured(t, RegisterLoginItem)

	if !strings.Contains(output, "Could not register the login item: not authorised") ||
		!strings.Contains(output, "Login Items") {
		t.Errorf("not explained:\n%s", output)
	}
}

func TestAPlainBuildIsNotInsideABundle(t *testing.T) {
	if bundle := enclosingBundle(); bundle != "" {
		t.Errorf("the test binary is inside %s", bundle)
	}
}
