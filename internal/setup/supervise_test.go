// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"os"
	"path/filepath"
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

	if err := SetDesiredState(DefaultProfileName, true); err != nil {
		t.Fatalf("recording up: %v", err)
	}
	if !desiredUp(DesiredStatePathFor(DefaultProfileName)) {
		t.Error("up was not read back")
	}

	if err := SetDesiredState(DefaultProfileName, false); err != nil {
		t.Fatalf("recording down: %v", err)
	}
	if desiredUp(DesiredStatePathFor(DefaultProfileName)) {
		t.Error("down was not read back")
	}

	// Written by the installer as root in one flow and by the menu bar as the
	// user in another; the menu bar has to be able to overwrite it.
	info, err := os.Stat(DesiredStatePathFor(DefaultProfileName))
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

// The daemon runs as root and cannot work out which of the machine's users owns
// the deployments, so the store's path has to be baked into the job.
func TestSupervisorRefusesAnUnnamedProfileStore(t *testing.T) {
	if err := InstallSupervisor(""); err == nil {
		t.Error("a supervisor with no profile store to watch was installed")
	}
}

// A profile that did not ask to be supervised must not be reconciled: the
// daemon holding a tunnel its owner never asked it to hold is the behaviour
// this product deliberately does not have.
func TestOnlySupervisedProfilesAreReconciled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	watched := valid()
	watched.ProfileName = "watched"
	watched.InterfaceName = "wg4"
	watched.Supervise = true
	writeProfileForTest(t, watched)

	ignored := valid()
	ignored.ProfileName = "ignored"
	ignored.InterfaceName = "wg5"
	ignored.ClientAddress = "10.101.0.2"
	ignored.Supervise = false
	writeProfileForTest(t, ignored)

	targets := superviseTargets(SuperviseOptions{ProfilesDir: ProfilesDir()})
	if len(targets) != 1 {
		t.Fatalf("%d tunnels would be reconciled, want 1: %+v", len(targets), targets)
	}
	if targets[0].profileName != "watched" {
		t.Errorf("the reconciled profile is %q, want \"watched\"", targets[0].profileName)
	}
	if targets[0].configPath != "/etc/wireguard/wg4.conf" {
		t.Errorf("it would raise %s", targets[0].configPath)
	}
}

// writeProfileForTest installs a profile the way the installer does: both the
// settings and the app config, because ListProfiles only counts a directory
// that holds the second one.
func writeProfileForTest(t *testing.T, s Settings) {
	t.Helper()
	if err := WriteAppConfig(s); err != nil {
		t.Fatalf("writing the app config for %s: %v", s.ProfileName, err)
	}
	if err := SaveProfileSettings(s); err != nil {
		t.Fatalf("writing the settings for %s: %v", s.ProfileName, err)
	}
}

// The daemon is root and its home is not the owner's. Before this, --profiles
// was logged and then ignored: every lookup resolved root's own empty store,
// and no tunnel was ever supervised.
func TestSupervisorReadsTheStoreItWasGivenNotRootsHome(t *testing.T) {
	owner := t.TempDir()
	t.Setenv("HOME", owner)
	if err := SaveProfileSettings(Settings{ProfileName: "work", InterfaceName: "wg3", Supervise: true}); err != nil {
		t.Fatal(err)
	}
	store := ProfilesDir()

	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { profilesDirOverride = "" })
	targets := superviseTargets(SuperviseOptions{ProfilesDir: store})
	if len(targets) != 0 {
		t.Fatalf("resolved %d targets before the store was applied; the test is not isolating HOME", len(targets))
	}

	Supervise(SuperviseOptions{ProfilesDir: store, Once: true})
	targets = superviseTargets(SuperviseOptions{ProfilesDir: store})
	if len(targets) != 1 || targets[0].interfaceName != "wg3" ||
		targets[0].statePath != filepath.Join(store, "work", "desired-state") {
		t.Fatalf("the supervisor did not read the store it was given: %+v", targets)
	}
}
