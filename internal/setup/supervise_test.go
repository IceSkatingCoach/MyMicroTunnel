// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"os"
	"strings"
	"testing"
)

func TestDesiredStateDefaultsToLeavingTheTunnelAlone(t *testing.T) {
	// No file means nobody has asked for anything. Reading that as "up" would
	// turn a fresh install into a tunnel nobody switched on.
	if desiredUp("/nonexistent/desired-state") {
		t.Error("a missing state file was read as a request to connect")
	}
}

func TestDesiredStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := SetDesiredState(true); err != nil {
		t.Fatalf("recording up: %v", err)
	}
	if !desiredUp(DesiredStatePath()) {
		t.Error("up was not read back")
	}

	if err := SetDesiredState(false); err != nil {
		t.Fatalf("recording down: %v", err)
	}
	if desiredUp(DesiredStatePath()) {
		t.Error("down was not read back")
	}

	// Written by the installer as root in one flow and by the menu bar as the
	// user in another; the menu bar has to be able to overwrite it.
	info, err := os.Stat(DesiredStatePath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("the state file is mode %o, want 644", mode)
	}
}

func TestDesiredStateIgnoresTrailingWhitespace(t *testing.T) {
	path := t.TempDir() + "/state"
	if err := os.WriteFile(path, []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !desiredUp(path) {
		t.Error("a trailing newline was read as a different state")
	}
}

func TestSupervisorPlistNamesThisDeployment(t *testing.T) {
	s := valid()
	s.InterfaceName = "wg3"
	s.ClientAddress = "10.100.0.5"

	plist := SupervisorPlist(s, HelperPath, "/tmp/desired-state")

	for _, expected := range []string{
		"<string>" + SupervisorLabel + "</string>",
		"<string>" + HelperPath + "</string>",
		"<string>supervise</string>",
		"<string>/tmp/desired-state</string>",
		"<string>wg3</string>",
		"<string>/etc/wireguard/wg3.conf</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(plist, expected) {
			t.Errorf("the plist has no %q:\n%s", expected, plist)
		}
	}
}

// The daemon runs as root and cannot work out which of the machine's users owns
// the deployment, so the path has to be baked into the job.
func TestSupervisorPlistRefusesAnUnnamedStateFile(t *testing.T) {
	if err := InstallSupervisor(valid(), ""); err == nil {
		t.Error("a supervisor with no state file to watch was installed")
	}
}
