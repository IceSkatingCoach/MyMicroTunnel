// SPDX-License-Identifier: GPL-3.0-or-later
package ui

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture redirects stdout and stderr to files for the length of the test and
// returns a function that reads what was written to each.
func capture(t *testing.T) func() (stdout, stderr string) {
	t.Helper()
	directory := t.TempDir()
	open := func(name string) *os.File {
		file, err := os.Create(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		return file
	}
	out, errOut := open("stdout"), open("stderr")

	previousOut, previousErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errOut
	t.Cleanup(func() { os.Stdout, os.Stderr = previousOut, previousErr })

	read := func(file *os.File) string {
		content, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(content)
	}
	return func() (string, string) { return read(out), read(errOut) }
}

func jsonOutput(t *testing.T, enabled bool) {
	t.Helper()
	previous := JSONMode()
	SetJSON(enabled)
	t.Cleanup(func() { SetJSON(previous) })
}

// typed makes the next prompts read the given input.
func typed(t *testing.T, input string) {
	t.Helper()
	previous := reader
	reader = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { reader = previous })
}

// exited is what the fake exit panics with, so a test can see that Fail ended
// the run and with which code, without the test binary ending with it.
type exited int

func failing(t *testing.T) {
	t.Helper()
	previous := exit
	exit = func(code int) { panic(exited(code)) }
	t.Cleanup(func() { exit = previous })
}

// run calls f and reports the exit code if it ended the run.
func run(f func()) (code int, ended bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			value, ok := recovered.(exited)
			if !ok {
				panic(recovered)
			}
			code, ended = int(value), true
		}
	}()
	f()
	return 0, false
}

func TestProgressLinesOnATerminal(t *testing.T) {
	jsonOutput(t, false)
	output := capture(t)

	Step("Creating %s", "the stack")
	Done("%d instances", 2)
	Warn("slow")
	Info("region %s", "eu-west-1")
	Result(map[string]string{"endpoint": "203.0.113.10"})

	stdout, stderr := output()
	want := "\n▸ Creating the stack\n" +
		"  ✓ 2 instances\n" +
		"  ! slow\n" +
		"  region eu-west-1\n"
	if stdout != want {
		t.Errorf("stdout is %q, want %q", stdout, want)
	}
	// On a terminal the result has already been printed as progress lines.
	if stderr != "" {
		t.Errorf("stderr is %q", stderr)
	}
}

func TestProgressLinesAreOneJSONEventEachForTheSetupWindow(t *testing.T) {
	jsonOutput(t, true)
	output := capture(t)

	Step("Creating %s", "the stack")
	Done("%d instances", 2)
	Warn("slow")
	Info("region %s", "eu-west-1")
	Result(map[string]string{"endpoint": "203.0.113.10"})

	stdout, _ := output()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	want := []struct{ kind, message string }{
		{"step", "Creating the stack"},
		{"done", "2 instances"},
		{"warn", "slow"},
		{"info", "region eu-west-1"},
	}
	if len(lines) != len(want)+1 {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want)+1, stdout)
	}
	for index, expected := range want {
		var got event
		if err := json.Unmarshal([]byte(lines[index]), &got); err != nil {
			t.Fatalf("line %d is not JSON: %q", index, lines[index])
		}
		if got.Kind != expected.kind || got.Message != expected.message {
			t.Errorf("line %d is %+v, want %+v", index, got, expected)
		}
	}

	var result struct {
		Kind string            `json:"kind"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[len(want)]), &result); err != nil {
		t.Fatalf("the result is not JSON: %q", lines[len(want)])
	}
	if result.Kind != "result" || result.Data["endpoint"] != "203.0.113.10" {
		t.Errorf("result is %+v", result)
	}
}

func TestFailSaysWhyAndEndsTheRun(t *testing.T) {
	jsonOutput(t, false)
	output := capture(t)
	failing(t)

	code, ended := run(func() { Fail("the stack is %s", "ROLLBACK_COMPLETE") })
	if !ended || code != 1 {
		t.Fatalf("Fail ended the run %v with code %d, want exit 1", ended, code)
	}
	stdout, stderr := output()
	if stderr != "\n✗ the stack is ROLLBACK_COMPLETE\n" {
		t.Errorf("stderr is %q", stderr)
	}
	if stdout != "" {
		t.Errorf("the failure went to stdout: %q", stdout)
	}
}

func TestFailIsAJSONEventForTheSetupWindow(t *testing.T) {
	jsonOutput(t, true)
	output := capture(t)
	failing(t)

	code, ended := run(func() { Fail("no %s", "credentials") })
	if !ended || code != 1 {
		t.Fatalf("Fail ended the run %v with code %d, want exit 1", ended, code)
	}
	stdout, stderr := output()
	if strings.TrimSpace(stdout) != `{"kind":"fail","message":"no credentials"}` {
		t.Errorf("stdout is %q", stdout)
	}
	if stderr != "" {
		t.Errorf("the setup window's failure also went to stderr: %q", stderr)
	}
}

func TestAskReturnsTheAnswerOrTheDefault(t *testing.T) {
	output := capture(t)
	typed(t, "  eu-west-1  \n\n")

	if got := Ask("Region", "us-east-1"); got != "eu-west-1" {
		t.Errorf("Ask = %q, want the trimmed answer", got)
	}
	if got := Ask("Region", "us-east-1"); got != "us-east-1" {
		t.Errorf("Ask = %q, want the default for an empty answer", got)
	}
	if stdout, _ := output(); stdout != "  Region [us-east-1]:   Region [us-east-1]: " {
		t.Errorf("the prompt is %q", stdout)
	}
}

func TestConfirmReadsYesNoAndTheDefault(t *testing.T) {
	output := capture(t)
	typed(t, "y\nYes\nn\nmaybe\n\n\n")

	for index, want := range []bool{true, true, false, false} {
		if got := Confirm("Continue?", false); got != want {
			t.Errorf("answer %d: Confirm = %v, want %v", index, got, want)
		}
	}
	if !Confirm("Continue?", true) {
		t.Error("an empty answer did not take the default of yes")
	}
	if Confirm("Continue?", false) {
		t.Error("an empty answer did not take the default of no")
	}

	stdout, _ := output()
	if !strings.Contains(stdout, "  Continue? (y/N): ") || !strings.Contains(stdout, "  Continue? (Y/n): ") {
		t.Errorf("the prompts do not show the default: %q", stdout)
	}
}

func TestAPromptWithNothingToReadEndsTheRun(t *testing.T) {
	jsonOutput(t, false)
	failing(t)

	for name, prompt := range map[string]func(){
		"Ask":     func() { Ask("Region", "us-east-1") },
		"Confirm": func() { Confirm("Continue?", true) },
	} {
		t.Run(name, func(t *testing.T) {
			output := capture(t)
			typed(t, "")

			// Taking the default here would answer a question nobody saw.
			code, ended := run(prompt)
			if !ended || code != 1 {
				t.Fatalf("ended %v with code %d, want exit 1", ended, code)
			}
			if _, stderr := output(); !strings.Contains(stderr, "Could not read input: EOF") {
				t.Errorf("stderr is %q", stderr)
			}
		})
	}
}

func TestAskSecretRefusesInputThatIsNotATerminal(t *testing.T) {
	jsonOutput(t, false)
	output := capture(t)
	failing(t)

	// A pipe can be read, but not with echo turned off, so a secret piped in
	// has to be refused rather than read with echo on.
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readEnd.Close()
	_, _ = io.WriteString(writeEnd, "secret\n")
	writeEnd.Close()
	previous := os.Stdin
	os.Stdin = readEnd
	t.Cleanup(func() { os.Stdin = previous })

	code, ended := run(func() { AskSecret("Secret access key") })
	if !ended || code != 1 {
		t.Fatalf("ended %v with code %d, want exit 1", ended, code)
	}
	stdout, stderr := output()
	if stdout != "  Secret access key: \n" {
		t.Errorf("stdout is %q", stdout)
	}
	if !strings.Contains(stderr, "Could not read input") || strings.Contains(stdout+stderr, "secret\n") {
		t.Errorf("stderr is %q", stderr)
	}
}
