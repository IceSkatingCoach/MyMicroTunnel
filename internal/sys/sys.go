// SPDX-License-Identifier: GPL-3.0-or-later
// Package sys wraps the parts of the install that are not AWS: running local
// commands, and writing the two files that must be owned by root.
package sys

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Result struct {
	ExitCode int
	Output   string
}

func (r Result) OK() bool { return r.ExitCode == 0 }

// Run merges stdout and stderr, because the messages worth showing (sudo
// refusals, wg-quick errors) arrive on stderr.
func Run(name string, args ...string) Result {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()

	code := 0
	if err != nil {
		code = command.ProcessState.ExitCode()
		if code == 0 {
			code = -1
		}
	}
	return Result{ExitCode: code, Output: strings.TrimSpace(string(output))}
}

func RunWithInput(input string, name string, args ...string) Result {
	command := exec.Command(name, args...)
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()

	code := 0
	if err != nil {
		code = command.ProcessState.ExitCode()
		if code == 0 {
			code = -1
		}
	}
	return Result{ExitCode: code, Output: strings.TrimSpace(string(output))}
}

// RunInteractive hands the terminal over, for the commands that may ask for a
// password or that take long enough to want live output.
func RunInteractive(name string, args ...string) int {
	command := exec.Command(name, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if code := command.ProcessState.ExitCode(); code != 0 {
			return code
		}
		return -1
	}
	return 0
}

func Which(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WriteAsRoot stages the content in a private temporary file and moves it into
// place with `install`, so the destination's owner and mode are set by the same
// step that writes it. A file holding a private key never exists readable, not
// even briefly.
func WriteAsRoot(content, destination, mode string) error {
	scratch, err := os.MkdirTemp("", "wiregard-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	staged := filepath.Join(scratch, "staged")
	if err := os.WriteFile(staged, []byte(content), 0o600); err != nil {
		return err
	}

	if code := RunInteractive("/usr/bin/sudo", "install", "-m", mode, "-o", "root", "-g", "wheel", staged, destination); code != 0 {
		return fmt.Errorf("sudo install to %s exited %d", destination, code)
	}
	return nil
}

// WriteAsRootNonInteractive is the same write for contexts with no terminal to
// prompt on, such as the privileged step the setup window triggers. It assumes
// the process is already running as root.
func WriteAsRootNonInteractive(content, destination string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(destination, []byte(content), mode); err != nil {
		return err
	}
	if err := os.Chmod(destination, mode); err != nil {
		return err
	}
	return os.Chown(destination, 0, 0)
}

// homebrewDirs are searched in addition to PATH. A GUI app is launched by
// launchd, not by a shell, so it inherits a bare PATH of /usr/bin:/bin:
// /usr/sbin:/sbin — Homebrew's directory is not on it, and a tool that is
// plainly installed looks missing.
var homebrewDirs = []string{
	"/opt/homebrew/bin", // Apple silicon
	"/usr/local/bin",    // Intel, and older installs
}

// Tool resolves a command to an absolute path, falling back to the well-known
// Homebrew locations when PATH does not have it.
func Tool(name string) string {
	if path := Which(name); path != "" {
		return path
	}
	for _, directory := range homebrewDirs {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// ExtendPath adds the Homebrew directories to this process's PATH, so child
// processes inherit them too. Called once at startup.
func ExtendPath() {
	current := os.Getenv("PATH")
	for _, directory := range homebrewDirs {
		if !strings.Contains(current, directory) {
			current = current + ":" + directory
		}
	}
	os.Setenv("PATH", current)
}
