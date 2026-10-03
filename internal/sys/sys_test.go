// SPDX-License-Identifier: GPL-3.0-or-later
package sys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script writes an executable shell script into a directory of its own and
// returns its path.
func script(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunMergesStdoutAndStderr(t *testing.T) {
	// sudo refusals and wg-quick errors arrive on stderr; they are the part
	// worth showing.
	tool := script(t, "tool", "echo \"out $1 $2\"\necho 'err' >&2\n")

	result := Run(tool, "first", "second")
	if !result.OK() || result.ExitCode != 0 {
		t.Errorf("result = %+v, want success", result)
	}
	if result.Output != "out first second\nerr" {
		t.Errorf("output is %q", result.Output)
	}
}

func TestRunReportsTheExitCode(t *testing.T) {
	tool := script(t, "tool", "echo '  refused  '\nexit 3\n")

	result := Run(tool)
	if result.OK() || result.ExitCode != 3 {
		t.Errorf("result = %+v, want exit 3", result)
	}
	if result.Output != "refused" {
		t.Errorf("output is %q, want it trimmed", result.Output)
	}
}

func TestRunOfACommandThatDoesNotExistFails(t *testing.T) {
	result := Run(filepath.Join(t.TempDir(), "missing"))
	if result.OK() {
		t.Errorf("result = %+v, want a failure", result)
	}
}

func TestRunWithInputFeedsStdin(t *testing.T) {
	tool := script(t, "tool", "read line\necho \"got $line\"\nexit 4\n")

	result := RunWithInput("the input\n", tool)
	if result.ExitCode != 4 || result.Output != "got the input" {
		t.Errorf("result = %+v", result)
	}

	if missing := RunWithInput("", filepath.Join(t.TempDir(), "missing")); missing.OK() {
		t.Errorf("result = %+v, want a failure", missing)
	}
}

func TestRunInteractiveReturnsTheExitCode(t *testing.T) {
	cases := map[string]int{"exit 0\n": 0, "exit 5\n": 5}
	for body, want := range cases {
		if got := RunInteractive(script(t, "tool", body)); got != want {
			t.Errorf("%q: RunInteractive = %d, want %d", body, got, want)
		}
	}
	if got := RunInteractive(filepath.Join(t.TempDir(), "missing")); got == 0 {
		t.Error("a command that does not exist was reported as success")
	}
}

func TestWhichAndToolFindWhatIsOnPath(t *testing.T) {
	tool := script(t, "mmt-tool", "exit 0\n")
	t.Setenv("PATH", filepath.Dir(tool))

	if got := Which("mmt-tool"); got != tool {
		t.Errorf("Which = %q, want %q", got, tool)
	}
	if got := Tool("mmt-tool"); got != tool {
		t.Errorf("Tool = %q, want %q", got, tool)
	}
	if got := Which("mmt-missing"); got != "" {
		t.Errorf("Which found %q for a tool that is not there", got)
	}
}

func withHomebrew(t *testing.T, directories ...string) {
	t.Helper()
	previous := homebrewDirs
	homebrewDirs = directories
	t.Cleanup(func() { homebrewDirs = previous })
}

func TestToolLooksInHomebrewWhenPathDoesNotHaveIt(t *testing.T) {
	// A GUI app inherits a PATH without Homebrew on it.
	t.Setenv("PATH", t.TempDir())

	notExecutable := t.TempDir()
	if err := os.WriteFile(filepath.Join(notExecutable, "mmt-tool"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	directoryOfThatName := t.TempDir()
	if err := os.Mkdir(filepath.Join(directoryOfThatName, "mmt-tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	brewed := script(t, "mmt-tool", "exit 0\n")

	withHomebrew(t, t.TempDir(), notExecutable, directoryOfThatName, filepath.Dir(brewed))
	if got := Tool("mmt-tool"); got != brewed {
		t.Errorf("Tool = %q, want %q", got, brewed)
	}

	withHomebrew(t, notExecutable, directoryOfThatName)
	if got := Tool("mmt-tool"); got != "" {
		t.Errorf("Tool = %q for something that cannot be run", got)
	}
}

func TestExtendPathAddsHomebrewOnce(t *testing.T) {
	withHomebrew(t, "/opt/homebrew/bin", "/usr/local/bin")
	t.Setenv("PATH", "/usr/bin:/bin:/usr/local/bin")

	ExtendPath()
	ExtendPath()

	if got := os.Getenv("PATH"); got != "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin" {
		t.Errorf("PATH is %q", got)
	}
}

func TestExistsOnlyForAPathThatIsThere(t *testing.T) {
	directory := t.TempDir()
	if !Exists(directory) {
		t.Error("a directory that exists does not")
	}
	if Exists(filepath.Join(directory, "missing")) {
		t.Error("a file that does not exist does")
	}
}

func TestWriteAsRootInstallsAPrivateStagedCopyWithSudo(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	var (
		ran      string
		args     []string
		staged   string
		mode     os.FileMode
		lastSeen error
	)
	previous := runInteractive
	runInteractive = func(name string, arguments ...string) int {
		ran, args = name, arguments
		path := arguments[len(arguments)-2]
		content, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		lastSeen = errors.Join(err, statErr)
		if statErr == nil {
			staged, mode = string(content), info.Mode().Perm()
		}
		return 0
	}
	t.Cleanup(func() { runInteractive = previous })

	if err := WriteAsRoot("PrivateKey = secret\n", "/etc/wireguard/wg0.conf", "0600"); err != nil {
		t.Fatal(err)
	}
	if lastSeen != nil {
		t.Fatalf("the staged file was not there for install to read: %v", lastSeen)
	}

	if ran != "/usr/bin/sudo" {
		t.Errorf("ran %q, want sudo", ran)
	}
	want := []string{"install", "-m", "0600", "-o", "0", "-g", "0", args[len(args)-2], "/etc/wireguard/wg0.conf"}
	if fmt.Sprint(args) != fmt.Sprint(want) {
		t.Errorf("sudo %q, want sudo %q", args, want)
	}
	if staged != "PrivateKey = secret\n" {
		t.Errorf("the staged file holds %q", staged)
	}
	// The key is never readable by anyone else, not even before install runs.
	if mode != 0o600 {
		t.Errorf("the staged file is mode %o, want 600", mode)
	}
	if _, err := os.Stat(args[len(args)-2]); !errors.Is(err, os.ErrNotExist) {
		t.Error("the staged copy of the secret was left behind")
	}
}

func TestWriteAsRootReportsAnInstallThatFailed(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	previous := runInteractive
	runInteractive = func(string, ...string) int { return 1 }
	t.Cleanup(func() { runInteractive = previous })

	err := WriteAsRoot("x", "/etc/wireguard/wg0.conf", "0600")
	if err == nil || !strings.Contains(err.Error(), "/etc/wireguard/wg0.conf exited 1") {
		t.Errorf("err = %v, want one naming the destination and the exit code", err)
	}
}

func TestWriteAsRootNeedsSomewhereToStage(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	previous := runInteractive
	runInteractive = func(string, ...string) int {
		t.Error("sudo ran with nothing staged")
		return 0
	}
	t.Cleanup(func() { runInteractive = previous })

	if err := WriteAsRoot("x", "/etc/wireguard/wg0.conf", "0600"); err == nil {
		t.Error("a missing temporary directory was not reported")
	}
}

func TestWriteAsRootNonInteractiveWritesTheFileAndHandsItToRoot(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "wireguard", "wg0.conf")

	err := WriteAsRootNonInteractive("PrivateKey = secret\n", destination, 0o600)
	// Handing the file to root needs root; everything before it does not.
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, os.ErrPermission) {
		t.Errorf("err = %v, want the chown to root refused", err)
	}

	content, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "PrivateKey = secret\n" {
		t.Errorf("the file holds %q", content)
	}
	info, _ := os.Stat(destination)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the file is mode %o, want 600", info.Mode().Perm())
	}
	if parent, _ := os.Stat(filepath.Dir(destination)); parent.Mode().Perm() != 0o700 {
		t.Errorf("the directory it made is mode %o, want 700", parent.Mode().Perm())
	}
}

func TestWriteAsRootNonInteractiveReportsWhereItCouldNotWrite(t *testing.T) {
	directory := t.TempDir()
	blocker := filepath.Join(directory, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The parent is a file, so the directory cannot be made.
	if err := WriteAsRootNonInteractive("x", filepath.Join(blocker, "wg0.conf"), 0o600); err == nil {
		t.Error("a destination under a file was written")
	}
	// The destination is a directory, so it cannot be written.
	if err := WriteAsRootNonInteractive("x", directory, 0o600); err == nil {
		t.Error("a directory was overwritten")
	}
}
