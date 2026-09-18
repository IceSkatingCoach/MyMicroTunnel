// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// daemonising writes a script that behaves the way wireguard-go does: it says
// something, forks a child that outlives it, and exits. The child inherits
// whatever stdout and stderr it was given and holds them open.
func daemonising(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "daemonise.sh")
	script := "#!/bin/bash\n" +
		"echo 'starting up'\n" +
		// The child keeps the inherited descriptors open for a minute, which is
		// far longer than this test is willing to wait.
		"sleep 60 &\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// This is the bug that a hundred passing unit tests did not find and the first
// real tunnel did: exec.Command(...).CombinedOutput() attaches pipes and waits
// for EOF, the forked child inherits those pipes, and the call blocks for as
// long as the daemon runs. The tunnel came up correctly and the installer
// waited for it forever.
func TestRunDetachedDoesNotWaitForTheDaemon(t *testing.T) {
	script := daemonising(t)

	finished := make(chan struct{})
	var output string
	var err error

	go func() {
		output, err = runDetached(script, nil)
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("runDetached is still waiting for a process that has already exited; " +
			"it is waiting on descriptors the daemonised child inherited")
	}

	if err != nil {
		t.Fatalf("the script failed: %v", err)
	}
	// Whatever it managed to say before forking still has to come back, because
	// that is where the reason for a refusal to start appears.
	if output != "starting up" {
		t.Errorf("output is %q, want %q", output, "starting up")
	}
}

func TestRunDetachedReportsAFailureToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refuses.sh")
	script := "#!/bin/bash\necho 'cannot open utun' >&2\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	output, err := runDetached(path, nil)
	if err == nil {
		t.Fatal("a script that exited 1 was reported as success")
	}
	if output != "cannot open utun" {
		t.Errorf("stderr did not come back: %q", output)
	}
}

func TestRunDetachedPassesTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.sh")
	if err := os.WriteFile(path, []byte("#!/bin/bash\necho \"$WG_TUN_NAME_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The name file is how the caller learns which utun the kernel handed out,
	// so an environment that does not reach the process is a tunnel that can
	// never be found again.
	output, err := runDetached(path, []string{"WG_TUN_NAME_FILE=/var/run/wireguard/wg0.name"})
	if err != nil {
		t.Fatal(err)
	}
	if output != "/var/run/wireguard/wg0.name" {
		t.Errorf("the environment did not reach the process: %q", output)
	}
}
