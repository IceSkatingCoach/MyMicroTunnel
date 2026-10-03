// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// invocation is one command the code under test asked to run.
type invocation struct {
	name string
	args []string
}

func (i invocation) String() string {
	return filepath.Base(i.name) + " " + strings.Join(i.args, " ")
}

// fakeCommands stands in for execCommand. Every command is recorded, and
// respond decides what it prints and how it exits. What actually runs is a
// one-line shell script, so the code under test gets a real *exec.Cmd with real
// output, a real exit status and a real stdin. The shell rather than this test
// binary re-executed: on macOS that costs over a second per command.
type fakeCommands struct {
	t       *testing.T
	dir     string
	respond func(call invocation) (output string, code int)

	mu    sync.Mutex
	calls []invocation
}

func fakeCommandsFor(t *testing.T, respond func(call invocation) (string, int)) *fakeCommands {
	t.Helper()
	fake := &fakeCommands{t: t, dir: t.TempDir(), respond: respond}
	previous := execCommand
	execCommand = fake.command
	t.Cleanup(func() { execCommand = previous })
	return fake
}

func (f *fakeCommands) command(name string, args ...string) *exec.Cmd {
	call := invocation{name: name, args: append([]string(nil), args...)}
	output, code := "", 0
	if f.respond != nil {
		output, code = f.respond(call)
	}

	f.mu.Lock()
	index := len(f.calls)
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	command := exec.Command("/bin/sh", "-c",
		`cat > "$MMT_HELPER_STDIN"; printf '%s' "$MMT_HELPER_OUTPUT"; exit "$MMT_HELPER_EXIT"`)
	command.Env = append(os.Environ(),
		"MMT_HELPER_OUTPUT="+output,
		"MMT_HELPER_EXIT="+strconv.Itoa(code),
		"MMT_HELPER_STDIN="+f.stdinPath(index),
	)
	return command
}

func (f *fakeCommands) stdinPath(index int) string {
	return filepath.Join(f.dir, fmt.Sprintf("stdin-%d", index))
}

// issued is every command run so far, spelled the way a person would type it.
func (f *fakeCommands) issued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		lines = append(lines, call.String())
	}
	return lines
}

// stdin is what the index'th command was given to read.
func (f *fakeCommands) stdin(index int) string {
	f.t.Helper()
	content, err := os.ReadFile(f.stdinPath(index))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(content)
}

func succeed(invocation) (string, int) { return "", 0 }

// testConfig has a blank range among the real ones, which Up has to skip.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		PrivateKey: key(t),
		Peers: []Peer{{
			PublicKey:  key(t),
			Endpoint:   "203.0.113.10:51820",
			AllowedIPs: []string{"10.100.0.0/24", " ", "172.31.0.0/16"},
		}},
	}
}

func expectCommands(t *testing.T, fake *fakeCommands, want ...string) {
	t.Helper()
	got := fake.issued()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands issued:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
