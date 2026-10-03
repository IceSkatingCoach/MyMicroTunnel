// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The test binary doubles as /usr/bin/security: run with MMT_FAKE_SECURITY
// set, it keeps generic passwords as files in that directory instead of
// touching the login Keychain.
func TestMain(m *testing.M) {
	if directory := os.Getenv("MMT_FAKE_SECURITY"); directory != "" {
		os.Exit(fakeSecurity(directory, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSecurity(directory string, args []string) int {
	if failure := os.Getenv("MMT_FAKE_SECURITY_FAIL"); failure != "" {
		fmt.Println(failure)
		return 1
	}
	log, _ := os.OpenFile(filepath.Join(directory, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintln(log, strings.Join(args, " "))
	log.Close()

	if len(args) == 0 {
		return 2
	}
	option := func(name string) string {
		if index := slices.Index(args, name); index >= 0 && index+1 < len(args) {
			return args[index+1]
		}
		return ""
	}
	item := filepath.Join(directory, option("-s")+"."+option("-a"))
	const missing = "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."

	switch args[0] {
	case "add-generic-password":
		if err := os.WriteFile(item, []byte(option("-w")), 0o600); err != nil {
			return 1
		}
	case "find-generic-password":
		value, err := os.ReadFile(item)
		if err != nil {
			fmt.Fprintln(os.Stderr, missing)
			return 44
		}
		fmt.Println(string(value))
	case "delete-generic-password":
		if err := os.Remove(item); err != nil {
			fmt.Println(missing)
			return 44
		}
	default:
		return 2
	}
	return 0
}

// isolateCredentialStore stands the test binary in for /usr/bin/security and
// returns the directory its items live in.
func isolateCredentialStore(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	t.Setenv("MMT_FAKE_SECURITY", directory)
	previous := securityTool
	securityTool = executable
	t.Cleanup(func() { securityTool = previous })
	return directory
}

func breakCredentialStore(t *testing.T) {
	t.Helper()
	t.Setenv("MMT_FAKE_SECURITY_FAIL", "security: User interaction is not allowed.")
}

func keychainCalls(t *testing.T, directory string) []string {
	t.Helper()
	content, _ := os.ReadFile(filepath.Join(directory, "calls"))
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

func TestKeychainCredentialsRoundTrip(t *testing.T) {
	directory := isolateCredentialStore(t)

	if stored, err := LoadAppCredentials("123456789012"); stored != nil || err != nil {
		t.Fatalf("nothing stored yet, got %v, %v", stored, err)
	}
	if err := StoreAppCredentials("123456789012", &AppCredentials{AccessKeyID: "AKIA", SecretAccessKey: "s:e"}); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadAppCredentials("123456789012")
	if err != nil || stored == nil || stored.AccessKeyID != "AKIA" || stored.SecretAccessKey != "s:e" {
		t.Fatalf("got %+v, %v", stored, err)
	}
	if err := ForgetAppCredentials("123456789012"); err != nil {
		t.Fatal(err)
	}
	if stored, _ := LoadAppCredentials("123456789012"); stored != nil {
		t.Error("forgotten credentials are still readable")
	}
	// Forgetting twice is what an uninstall run twice does.
	if err := ForgetAppCredentials("123456789012"); err != nil {
		t.Errorf("forgetting a missing item: %v", err)
	}

	add := keychainCalls(t, directory)[1]
	// -U is what lets a re-minted key replace a revoked one.
	for _, want := range []string{"add-generic-password", "-a 123456789012", "-s " + keychainService, " -U "} {
		if !strings.Contains(add, want) {
			t.Errorf("the add has no %q: %s", want, add)
		}
	}
}

func TestKeychainRefusesAnIncompleteCredential(t *testing.T) {
	directory := isolateCredentialStore(t)

	for _, credentials := range []*AppCredentials{nil, {AccessKeyID: "AKIA"}, {SecretAccessKey: "s"}} {
		if err := StoreAppCredentials("123456789012", credentials); err == nil {
			t.Errorf("stored %+v", credentials)
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "calls")); err == nil {
		t.Error("an incomplete credential reached the Keychain")
	}
}

func TestKeychainReportsAnItemThatIsNotAKeyPair(t *testing.T) {
	directory := isolateCredentialStore(t)
	if err := os.WriteFile(filepath.Join(directory, keychainService+".123456789012"), []byte("no-separator"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAppCredentials("123456789012"); err == nil || !strings.Contains(err.Error(), "not a key pair") {
		t.Errorf("got %v", err)
	}
}

func TestKeychainFailuresAreReported(t *testing.T) {
	isolateCredentialStore(t)
	breakCredentialStore(t)

	err := StoreAppCredentials("123456789012", &AppCredentials{AccessKeyID: "AKIA", SecretAccessKey: "s"})
	if err == nil || !strings.Contains(err.Error(), "security add-generic-password: security: User interaction is not allowed.") {
		t.Errorf("store: got %v", err)
	}
	if err := ForgetAppCredentials("123456789012"); err == nil || !strings.Contains(err.Error(), "security delete-generic-password") {
		t.Errorf("forget: got %v", err)
	}
}
