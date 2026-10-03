// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staged is a profile whose deploy stage left a fresh key for the privileged
// one to install.
func staged(t *testing.T, fake *fakeMachine) Settings {
	t.Helper()
	fake.reply("/usr/sbin/visudo", "parsed OK")
	s := valid()
	s.ServerPublicKey = "server-key"
	s.Endpoint = "203.0.113.10"
	s.StagedKeyPath = stagedKeyPath(s.ProfileName)
	if err := os.MkdirAll(filepath.Dir(s.StagedKeyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.StagedKeyPath, []byte("private-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(onDisk(path))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestThePrivilegedStageInstallsTheKeyTheConfigAndTheRule(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)

	if err := WriteRootFiles(s, "alice", true); err != nil {
		t.Fatalf("writing the root files: %v", err)
	}

	if key := fake.read(t, s.ClientKeyPath()); key != "private-key\n" {
		t.Errorf("the key is %q", key)
	}
	if config := fake.read(t, s.TunnelConfigPath()); config != TunnelConfig(s) {
		t.Errorf("the tunnel config is not the profile's:\n%s", config)
	}
	if rule := fake.read(t, SudoersPath); !strings.Contains(rule, "alice ALL=(root) NOPASSWD: "+HelperPath+" tunnel up wg0") {
		t.Errorf("the rule does not name alice:\n%s", rule)
	}
	for path, want := range map[string]os.FileMode{s.ClientKeyPath(): 0o600, s.TunnelConfigPath(): 0o600, SudoersPath: 0o440} {
		if got := mode(t, path); got != want {
			t.Errorf("%s is mode %o, want %o", path, got, want)
		}
	}
	if _, err := os.Stat(s.StagedKeyPath); !os.IsNotExist(err) {
		t.Errorf("the staged key outlived the install: %v", err)
	}
	if fake.ran("/usr/bin/sudo") {
		t.Errorf("root asked sudo: %v", fake.commands)
	}
}

func TestSudoUserIsWhoTheRuleNames(t *testing.T) {
	stubMachine(t)
	t.Setenv("SUDO_USER", "bob")
	if name := CurrentUsername(); name != "bob" {
		t.Errorf("the rule would name %q, want bob", name)
	}
}

// The sudoers file is machine-wide; writing it from one profile alone would
// revoke the others.
func TestTheRuleKeepsEveryOtherProfilesGrant(t *testing.T) {
	fake := stubMachine(t)
	other := valid()
	other.ProfileName = "home"
	other.InterfaceName = "wg1"
	writeProfileForTest(t, other)
	s := staged(t, fake)

	if err := WriteRootFiles(s, "alice", true); err != nil {
		t.Fatal(err)
	}
	rule := fake.read(t, SudoersPath)
	if !strings.Contains(rule, "tunnel up wg0") || !strings.Contains(rule, "tunnel up wg1") {
		t.Errorf("a profile lost its grant:\n%s", rule)
	}
}

func TestARuleVisudoRejectsIsNeverInstalled(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)
	fake.refuse("/usr/sbin/visudo", "syntax error near line 12")

	err := WriteRootFiles(s, "alice", true)
	if err == nil || !strings.Contains(err.Error(), "syntax error near line 12") {
		t.Fatalf("the rejection was not returned: %v", err)
	}
	if _, err := os.Stat(onDisk(SudoersPath)); !os.IsNotExist(err) {
		t.Errorf("a rejected rule was installed: %v", err)
	}
}

// Overwriting a key leaves the gateway trusting a public half whose private
// half no longer exists.
func TestAnExistingKeyIsNeverReplaced(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)
	fake.place(t, s.ClientKeyPath(), "the-original\n", 0o600)

	output := captured(t, func() {
		if err := WriteRootFiles(s, "alice", true); err != nil {
			t.Fatal(err)
		}
	})

	if key := fake.read(t, s.ClientKeyPath()); key != "the-original\n" {
		t.Errorf("the key was replaced with %q", key)
	}
	if !strings.Contains(output, "Keeping the existing "+s.ClientKeyPath()) {
		t.Errorf("the discarded key was not reported:\n%s", output)
	}
}

// A config naming a key nobody installed comes up and cannot load it, and every
// symptom points at the deployment instead.
func TestAProfileWithNoKeyAnywhereIsRefused(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)
	if err := os.Remove(s.StagedKeyPath); err != nil {
		t.Fatal(err)
	}

	err := WriteRootFiles(s, "alice", true)
	if err == nil || !strings.Contains(err.Error(), `VPN profile "default" has no private key at /etc/wireguard/wg0.key`) {
		t.Fatalf("not refused clearly: %v", err)
	}
	if _, err := os.Stat(onDisk(s.TunnelConfigPath())); !os.IsNotExist(err) {
		t.Errorf("a config naming no key was written: %v", err)
	}
}

func TestAFailedWriteStopsThePrivilegedStage(t *testing.T) {
	for _, asRoot := range []bool{true, false} {
		fake := stubMachine(t)
		s := staged(t, fake)
		fake.writeErrors[SudoersPath] = errors.New("read-only file system")

		if err := WriteRootFiles(s, "alice", asRoot); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Errorf("asRoot=%t: the failure was not returned: %v", asRoot, err)
		}
		if _, err := os.Stat(onDisk(s.ClientKeyPath())); !os.IsNotExist(err) {
			t.Errorf("asRoot=%t: writing went on after a failure: %v", asRoot, err)
		}
	}
}

// From a terminal every write goes through the one sudo the user has already
// answered, the supervisor's definition included.
func TestTheTerminalInstallWritesThroughSudo(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)
	s.Supervise = true

	if err := WriteRootFiles(s, "alice", false); err != nil {
		t.Fatalf("writing the root files: %v", err)
	}
	for _, write := range []string{
		"sudo install " + s.TunnelConfigPath() + " 600",
		"sudo install " + SudoersPath + " 440",
		"sudo install " + s.ClientKeyPath() + " 600",
		"sudo install " + SupervisorPath + " 644",
	} {
		if !fake.ran(write) {
			t.Errorf("%q was not done: %v", write, fake.commands)
		}
	}
	if definition := fake.read(t, SupervisorPath); !strings.Contains(definition, ProfilesDir()) {
		t.Errorf("the supervisor does not watch %s:\n%s", ProfilesDir(), definition)
	}
	if fake.ran("write ") {
		t.Errorf("wrote as root without sudo: %v", fake.commands)
	}
}

func TestASupervisorDefinitionThatCannotBeWrittenIsReported(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)
	s.Supervise = true
	fake.writeErrors[SupervisorPath] = errors.New("sudo refused")

	if err := WriteRootFiles(s, "alice", false); err == nil || !strings.Contains(err.Error(), "sudo refused") {
		t.Errorf("the failure was not returned: %v", err)
	}
}

func TestThePrivilegedStageInstallsTheSupervisorWhenAnyProfileWantsIt(t *testing.T) {
	fake := stubMachine(t)
	acceptSupervisor(fake)
	watched := valid()
	watched.ProfileName = "watched"
	watched.InterfaceName = "wg3"
	watched.Supervise = true
	writeProfileForTest(t, watched)
	s := staged(t, fake)

	if err := WriteRootFiles(s, "alice", true); err != nil {
		t.Fatal(err)
	}
	if definition := fake.read(t, SupervisorPath); !strings.Contains(definition, ProfilesDir()) {
		t.Errorf("the supervisor was not installed:\n%s", definition)
	}
}

// Turning supervision off has to actually turn it off.
func TestTheSupervisorIsRemovedWhenNoProfileWantsIt(t *testing.T) {
	fake := stubMachine(t)
	s := staged(t, fake)
	fake.place(t, SupervisorPath, "job", 0o644)

	captured(t, func() {
		if err := WriteRootFiles(s, "alice", true); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(onDisk(SupervisorPath)); !os.IsNotExist(err) {
		t.Errorf("the supervisor outlived the last profile that wanted it: %v", err)
	}
}

func TestAProfileIsReplacedNotDuplicatedInTheMachineWideList(t *testing.T) {
	stored := valid()
	stored.InterfaceName = "wg0"
	changed := stored
	changed.InterfaceName = "wg5"
	other := valid()
	other.ProfileName = "home"

	merged := withProfile([]Settings{stored, other}, changed)
	if len(merged) != 2 || merged[0].ProfileName != "home" || merged[1].InterfaceName != "wg5" {
		t.Errorf("merged %+v", merged)
	}
	if AnySupervised(merged) {
		t.Error("no profile asked to be supervised")
	}
	merged[0].Supervise = true
	if !AnySupervised(merged) {
		t.Error("a supervised profile was missed")
	}
}
