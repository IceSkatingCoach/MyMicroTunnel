// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

type exitSignal struct{ code int }

// runMain runs the command as `go run` would, with these arguments, and returns
// the code it exited with (0 when it returned) and what it printed.
func runMain(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	savedArgs, savedFlags := os.Args, flag.CommandLine
	defer func() { os.Args, flag.CommandLine = savedArgs, savedFlags }()

	return capture(t, func() {
		flag.CommandLine = flag.NewFlagSet("cmd", flag.ContinueOnError)
		os.Args = append([]string{"cmd"}, args...)
		main()
	})
}

// capture runs body with exit turned into a panic, so a failure unwinds back
// here instead of ending the test binary.
func capture(t *testing.T, body func()) (code int, stdout, stderr string) {
	t.Helper()
	directory := t.TempDir()
	outFile, err := os.Create(filepath.Join(directory, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	defer errFile.Close()

	savedOut, savedErr, savedExit := os.Stdout, os.Stderr, exit
	os.Stdout, os.Stderr = outFile, errFile
	exit = func(code int) { panic(exitSignal{code}) }
	defer func() { os.Stdout, os.Stderr, exit = savedOut, savedErr, savedExit }()

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				signal, ok := recovered.(exitSignal)
				if !ok {
					panic(recovered)
				}
				code = signal.code
			}
		}()
		body()
	}()

	out, _ := os.ReadFile(outFile.Name())
	errOut, _ := os.ReadFile(errFile.Name())
	return code, string(out), string(errOut)
}

// newRepository makes a directory that looks like the checkout's root and
// works from it.
func newRepository(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example\n")
	if version != "" {
		writeFile(t, filepath.Join(root, "VERSION"), version+"\n")
	}
	t.Chdir(root)
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
