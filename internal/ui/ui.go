// SPDX-License-Identifier: GPL-3.0-or-later
// Package ui is the installer's terminal conversation: progress lines, prompts
// and the one input that must not be echoed.
package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

var reader = bufio.NewReader(os.Stdin)

// jsonMode swaps the human output for one NDJSON event per line. The Swift
// setup window drives this same binary and renders those events, so the AWS
// logic exists once and the two front ends cannot drift apart.
var jsonMode bool

func SetJSON(enabled bool) { jsonMode = enabled }

func JSONMode() bool { return jsonMode }

type event struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func emit(kind, message string) bool {
	if !jsonMode {
		return false
	}
	encoded, err := json.Marshal(event{Kind: kind, Message: message})
	if err != nil {
		return true
	}
	fmt.Println(string(encoded))
	os.Stdout.Sync()
	return true
}

func Step(format string, args ...any) {
	if emit("step", fmt.Sprintf(format, args...)) {
		return
	}
	fmt.Printf("\n▸ "+format+"\n", args...)
}

func Done(format string, args ...any) {
	if emit("done", fmt.Sprintf(format, args...)) {
		return
	}
	fmt.Printf("  ✓ "+format+"\n", args...)
}

func Warn(format string, args ...any) {
	if emit("warn", fmt.Sprintf(format, args...)) {
		return
	}
	fmt.Printf("  ! "+format+"\n", args...)
}

func Info(format string, args ...any) {
	if emit("info", fmt.Sprintf(format, args...)) {
		return
	}
	fmt.Printf("  "+format+"\n", args...)
}

// Fail ends the run. Every failure path goes through here so a half-finished
// install always says which step stopped it rather than unwinding silently.
func Fail(format string, args ...any) {
	if emit("fail", fmt.Sprintf(format, args...)) {
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "\n✗ "+format+"\n", args...)
	os.Exit(1)
}

func Ask(question, fallback string) string {
	fmt.Printf("  %s [%s]: ", question, fallback)
	answer, err := reader.ReadString('\n')
	if err != nil {
		Fail("Could not read input: %v", err)
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return fallback
	}
	return answer
}

func Confirm(question string, fallback bool) bool {
	hint := "y/N"
	if fallback {
		hint = "Y/n"
	}
	fmt.Printf("  %s (%s): ", question, hint)
	answer, err := reader.ReadString('\n')
	if err != nil {
		Fail("Could not read input: %v", err)
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "" {
		return fallback
	}
	return strings.HasPrefix(answer, "y")
}

// AskSecret reads without echoing. A secret access key should not survive in
// the scrollback of a shared terminal or in a screen recording.
func AskSecret(question string) string {
	fmt.Printf("  %s: ", question)
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		Fail("Could not read input: %v", err)
	}
	return strings.TrimSpace(string(secret))
}

// Result reports structured output to whoever is driving the binary. Only the
// JSON front end consumes it; on a terminal the same facts have already been
// printed as progress lines.
func Result(data map[string]string) {
	if !jsonMode {
		return
	}
	payload := map[string]any{"kind": "result", "data": data}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Println(string(encoded))
	os.Stdout.Sync()
}
