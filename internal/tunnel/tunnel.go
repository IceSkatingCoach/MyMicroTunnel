// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunDir is where wireguard-go publishes the name of the utun device it got and
// the UAPI socket that controls it. The path is wg-quick's, so a machine that
// also has the Homebrew tools installed sees one consistent picture rather than
// two tunnels that do not know about each other.
const RunDir = "/var/run/wireguard"

// Options is one tunnel, as this machine runs it.
type Options struct {
	// Name is the logical name — wg0 — not the utun device the kernel hands
	// out. macOS does not let a tunnel choose its own device name, so every
	// interface here is a utun with a number nobody picked.
	Name string

	// Engine is the path to wireguard-go. Empty means look in the usual places.
	Engine string

	Address string
	MTU     int
	Config  Config
}

// IsUp reports whether the tunnel exists, which is not the same as whether it
// carries packets — for that, ask Status.
func IsUp(name string) bool { return Device(name) != "" }

// runDetached starts a process that daemonises, and collects whatever it said
// before it did.
//
// The obvious way to write this — exec.Command(...).CombinedOutput() — hangs
// forever. CombinedOutput attaches pipes and waits for them to reach EOF, and
// wireguard-go forks a child that inherits those pipes and holds them open for
// as long as the tunnel is up. The parent exits immediately, the daemon runs
// perfectly, and the caller waits for an EOF that will not arrive until the
// tunnel is torn down.
//
// A file has no such lifetime: Run waits for the process it started and nothing
// else, and the daemon inheriting the descriptor costs nothing.
func runDetached(name string, environment []string, args ...string) (string, error) {
	log, err := os.CreateTemp("", "detached-")
	if err != nil {
		return "", err
	}
	defer os.Remove(log.Name())
	defer log.Close()

	command := exec.Command(name, args...)
	command.Env = append(os.Environ(), environment...)
	command.Stdout = log
	command.Stderr = log

	runErr := command.Run()

	said, err := os.ReadFile(log.Name())
	if err != nil {
		said = nil
	}
	return strings.TrimSpace(string(said)), runErr
}

// --- finding wireguard-go --------------------------------------------------

// engineSearchPath is where a built product keeps its copy, in the order a
// running binary should trust them. PATH comes last: a Homebrew wireguard-go
// still works, but the one this package shipped with is the one it was tested
// against.
func engineSearchPath() []string {
	var candidates []string

	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		directory := filepath.Dir(executable)
		candidates = append(candidates,
			// Beside the engine, which is how the app bundle ships it and how
			// `make engine` lays out a source build.
			filepath.Join(directory, "wireguard-go"),
			filepath.Join(directory, "..", "Resources", "wireguard-go"),
			// A source checkout, from build/ or from the repository root.
			filepath.Join(directory, "..", "third_party", "wireguard-go"),
		)
	}

	return append(candidates,
		"/usr/local/lib/mymicrotunnel/wireguard-go",
		"/Applications/MyMicroTunnel.app/Contents/Resources/wireguard-go",
	)
}

// Engine resolves the path to wireguard-go, or explains what is missing.
func Engine(configured string) (string, error) {
	if configured != "" {
		if executable(configured) {
			return configured, nil
		}
		return "", fmt.Errorf("%s is not an executable file", configured)
	}

	for _, candidate := range engineSearchPath() {
		if executable(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	if found, err := exec.LookPath("wireguard-go"); err == nil {
		return found, nil
	}

	return "", errors.New("wireguard-go is missing. A packaged install ships it; " +
		"in a checkout, run `make wireguard`")
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
