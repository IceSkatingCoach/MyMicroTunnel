// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// shippedEngine is the copy Engine would find without being told, if this
// machine has one installed.
func shippedEngine() string {
	for _, candidate := range engineSearchPath() {
		if executable(candidate) {
			return filepath.Clean(candidate)
		}
	}
	return ""
}

func TestEngineUsesTheConfiguredPath(t *testing.T) {
	engine := writeExecutable(t, filepath.Join(t.TempDir(), "wireguard-go"))

	got, err := Engine(engine)
	if err != nil {
		t.Fatal(err)
	}
	if got != engine {
		t.Errorf("Engine(%q) = %q", engine, got)
	}
}

func TestEngineRefusesAConfiguredPathThatCannotRun(t *testing.T) {
	directory := t.TempDir()
	notExecutable := filepath.Join(directory, "wireguard-go")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Falling back to another copy here would run a binary the user did not
	// ask for and hide the typo in the one they did.
	for _, configured := range []string{notExecutable, directory, filepath.Join(directory, "missing")} {
		_, err := Engine(configured)
		if err == nil {
			t.Errorf("%s was accepted as the engine", configured)
			continue
		}
		if !strings.Contains(err.Error(), configured) {
			t.Errorf("the error does not name the path: %v", err)
		}
	}
}

func TestEnginePrefersTheShippedCopyAndFallsBackToPath(t *testing.T) {
	onPath := t.TempDir()
	writeExecutable(t, filepath.Join(onPath, "wireguard-go"))
	t.Setenv("PATH", onPath)

	got, err := Engine("")
	if err != nil {
		t.Fatal(err)
	}

	want := shippedEngine()
	if want == "" {
		want = filepath.Join(onPath, "wireguard-go")
	}
	if got != want {
		t.Errorf("Engine found %q, want %q", got, want)
	}
}

func TestEngineSaysHowToGetOneWhenNoneIsInstalled(t *testing.T) {
	if installed := shippedEngine(); installed != "" {
		t.Skipf("this machine has wireguard-go installed at %s", installed)
	}
	t.Setenv("PATH", t.TempDir())

	_, err := Engine("")
	if err == nil {
		t.Fatal("a missing wireguard-go was not reported")
	}
	if !strings.Contains(err.Error(), "make wireguard") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestEngineSearchPathTrustsTheShippedCopiesBeforePath(t *testing.T) {
	candidates := engineSearchPath()

	executablePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(executablePath); err == nil {
		executablePath = resolved
	}
	if want := filepath.Join(filepath.Dir(executablePath), "wireguard-go"); candidates[0] != want {
		t.Errorf("the first place searched is %q, want the copy beside the binary, %q", candidates[0], want)
	}

	tail := candidates[len(candidates)-2:]
	if tail[0] != "/usr/local/lib/mymicrotunnel/wireguard-go" ||
		tail[1] != "/Applications/MyMicroTunnel.app/Contents/Resources/wireguard-go" {
		t.Errorf("the installed locations are missing or out of order: %q", tail)
	}
}
