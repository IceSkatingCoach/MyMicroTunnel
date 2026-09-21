// SPDX-License-Identifier: GPL-3.0-or-later
package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end over the command itself: the binary is built and run, with a
// home directory of its own, exactly as a person would run it.
//
// Everything here avoids the two things a test cannot have — an AWS account
// and root — which still leaves the part that broke repeatedly in practice:
// what the stages hand each other, which VPN profile a command acts on, and
// whether a stage invents a deployment when it cannot find one. Every bug
// this file pins was found on a real machine, not by reading the code.

func build(t *testing.T) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "mymicrotunnel")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the command: %v\n%s", err, output)
	}
	return binary
}

// home gives the command a store of its own, so a test never reads or writes
// the profiles of whoever is running it.
func home(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func run(t *testing.T, binary, home string, args ...string) (string, int) {
	t.Helper()

	command := exec.Command(binary, args...)
	command.Env = append(os.Environ(), "HOME="+home)
	output, err := command.CombinedOutput()

	code := 0
	var exit *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &exit); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("running %v: %v", args, err)
		}
	}
	return string(output), code
}

func asExitError(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}

// A fresh machine holds no profiles, and says so rather than inventing one.
func TestProfileListOnAnEmptyMachine(t *testing.T) {
	binary, store := build(t), home(t)

	output, code := run(t, binary, store, "profile", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	if !strings.Contains(output, "No VPN profile is installed") {
		t.Errorf("a machine with no profiles reported:\n%s", output)
	}
}

// Saving records a profile and changes nothing in AWS. It is the button
// somebody adjusting a port presses, and it must be listable afterwards —
// being invisible read as the save having failed.
func TestSaveThenListAndShow(t *testing.T) {
	binary, store := build(t), home(t)

	output, code := run(t, binary, store, "profile", "save",
		"--vpn-profile", "lab",
		"--domain", "lab.example.com",
		"--port", "3000:8443",
		"--tcp-ports", "5432, 3000:8080",
		"--vpn-cidr", "10.110.0.0/24",
		"--idle-timeout", "30")
	if code != 0 {
		t.Fatalf("saving: exit %d: %s", code, output)
	}
	if !strings.Contains(output, filepath.Join("profiles", "lab", "settings.json")) {
		t.Errorf("save did not report where it wrote:\n%s", output)
	}

	listed, code := run(t, binary, store, "profile", "list")
	if code != 0 || !strings.Contains(listed, "lab") {
		t.Fatalf("a saved profile is not listed (exit %d):\n%s", code, listed)
	}

	shown, code := run(t, binary, store, "profile", "show", "lab")
	if code != 0 {
		t.Fatalf("show: exit %d: %s", code, shown)
	}
	for _, expected := range []string{
		"lab.example.com",
		"10.110.0.0/24",
		// Ports keep the shape they were typed in: local first, published
		// second, and a bare number for the two that agree.
		"5432, 3000:8080",
		"30 minutes",
	} {
		if !strings.Contains(shown, expected) {
			t.Errorf("show does not mention %q:\n%s", expected, shown)
		}
	}
	// The published half of the service port has to survive too: it decides
	// what the hostname answers on.
	if !strings.Contains(shown, "https://lab.example.com:8443") {
		t.Errorf("the published port is missing from the URL:\n%s", shown)
	}
}

// Two profiles must not be handed one interface, whether or not either has
// been deployed. Each was once invisible to the other's allocation, and the
// second one installed wrote its tunnel configuration over the first's.
func TestASecondProfileGetsItsOwnInterface(t *testing.T) {
	binary, store := build(t), home(t)

	for _, profile := range []struct{ name, cidr string }{
		{"first", "10.100.0.0/24"},
		{"second", "10.110.0.0/24"},
	} {
		output, code := run(t, binary, store, "profile", "save",
			"--vpn-profile", profile.name,
			"--domain", profile.name+".example.com",
			"--vpn-cidr", profile.cidr)
		if code != 0 {
			t.Fatalf("saving %s: %s", profile.name, output)
		}
	}

	listed, _ := run(t, binary, store, "profile", "list")
	interfaces := map[string]bool{}
	for _, line := range strings.Split(listed, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == "PROFILE" {
			continue
		}
		if interfaces[fields[1]] {
			t.Fatalf("two profiles share the interface %s:\n%s", fields[1], listed)
		}
		interfaces[fields[1]] = true
	}
	if len(interfaces) != 2 {
		t.Errorf("expected two interfaces, got %v:\n%s", interfaces, listed)
	}
}

// The addresses follow the subnet. Choosing one for a second profile used to
// be refused for two addresses the user had never typed.
func TestTunnelAddressesFollowTheChosenSubnet(t *testing.T) {
	binary, store := build(t), home(t)

	if output, code := run(t, binary, store, "profile", "save",
		"--vpn-profile", "lab", "--vpn-cidr", "10.110.0.0/24"); code != 0 {
		t.Fatalf("saving: %s", output)
	}

	shown, _ := run(t, binary, store, "profile", "show", "lab")
	if !strings.Contains(shown, "10.110.0.2") || !strings.Contains(shown, "10.110.0.1") {
		t.Errorf("the tunnel addresses did not follow the subnet:\n%s", shown)
	}
}

// A port that is not a port is refused where it is typed, not five minutes
// into a deploy as a CloudFormation parameter violation.
func TestNonsensePortsAreRefusedImmediately(t *testing.T) {
	binary, store := build(t), home(t)

	output, code := run(t, binary, store, "profile", "save",
		"--vpn-profile", "lab", "--tcp-ports", "http")
	if code == 0 {
		t.Fatalf("a non-numeric port was accepted:\n%s", output)
	}
	if !strings.Contains(output, "not a port") {
		t.Errorf("the refusal does not say what is wrong:\n%s", output)
	}
}

// The stages hand each other a settings file. A stage that applies one must
// refuse when it is not there rather than fall back to the built-in
// defaults: that invented a VPN profile called "default", took a free
// interface for it, and reported the result as an error about a profile
// nobody had created.
func TestAnApplyingStageRefusesAMissingSettingsFile(t *testing.T) {
	binary, store := build(t), home(t)
	missing := filepath.Join(store, "not-written.json")

	output, code := run(t, binary, store, "install",
		"--stage", "finish", "--non-interactive", "--settings", missing)
	if code == 0 {
		t.Fatalf("the finishing stage ran without the settings it applies:\n%s", output)
	}
	if !strings.Contains(output, "Cannot read") || !strings.Contains(output, "invent") {
		t.Errorf("the refusal does not explain itself:\n%s", output)
	}
	// And nothing was created behind it.
	if entries, err := os.ReadDir(filepath.Join(store, "Library", "Application Support",
		"MyMicroTunnel", "profiles")); err == nil && len(entries) > 0 {
		t.Errorf("a profile was invented anyway: %v", entries)
	}
}

// pubkey answers "is the key on this machine the one the gateway was told
// about" without printing the private half anywhere.
func TestPubkeyPrintsOnlyThePublicHalf(t *testing.T) {
	binary, store := build(t), home(t)

	// A key of known shape: 32 bytes, base64, as WireGuard writes them.
	private := "6KgQCVhLBOQoRHEPLLBGDh6xkrVwMIN0xCqEbGcnpVs="
	path := filepath.Join(store, "client.key")
	if err := os.WriteFile(path, []byte(private+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, code := run(t, binary, store, "pubkey", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	public := strings.TrimSpace(output)
	if len(public) != 44 || !strings.HasSuffix(public, "=") {
		t.Errorf("that is not a WireGuard public key: %q", public)
	}
	if strings.Contains(output, private) {
		t.Error("the private key was printed")
	}
}

// Every command that acts on a deployment takes --vpn-profile, and an
// unknown one is an error rather than a silent fall back to another
// profile's deployment.
func TestAnUnknownProfileIsAnError(t *testing.T) {
	binary, store := build(t), home(t)

	output, code := run(t, binary, store, "profile", "show", "nothing-here")
	if code == 0 {
		t.Fatalf("showing an unknown profile succeeded:\n%s", output)
	}
	if !strings.Contains(output, "No profile called") {
		t.Errorf("the error does not name the problem:\n%s", output)
	}
}
