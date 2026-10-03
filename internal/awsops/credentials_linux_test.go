// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxCredentialsRoundTripPrivately(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if stored, err := LoadAppCredentials("123456789012"); stored != nil || err != nil {
		t.Fatalf("nothing stored yet, got %v, %v", stored, err)
	}
	if err := StoreAppCredentials("123456789012", &AppCredentials{AccessKeyID: "AKIA", SecretAccessKey: "s:e"}); err != nil {
		t.Fatal(err)
	}
	path, _ := credentialPath("123456789012")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("expected a 0600 file, got %v, %v", info, err)
	}
	stored, err := LoadAppCredentials("123456789012")
	if err != nil || stored.AccessKeyID != "AKIA" || stored.SecretAccessKey != "s:e" {
		t.Fatalf("got %+v, %v", stored, err)
	}
	if err := ForgetAppCredentials("123456789012"); err != nil {
		t.Fatal(err)
	}
	if stored, _ := LoadAppCredentials("123456789012"); stored != nil {
		t.Error("forgotten credentials are still readable")
	}
	if _, err := credentialPath("../etc"); err == nil {
		t.Error("an account id must not be able to walk out of the store")
	}
}

func isolateCredentialStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// breakCredentialStore leaves the store nowhere to create its directory.
func breakCredentialStore(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxCredentialsRefuseWhatTheyCannotStoreSafely(t *testing.T) {
	isolateCredentialStore(t)

	for _, credentials := range []*AppCredentials{nil, {AccessKeyID: "AKIA"}, {SecretAccessKey: "s"}} {
		if err := StoreAppCredentials("123456789012", credentials); err == nil {
			t.Errorf("stored %+v", credentials)
		}
	}
	if err := StoreAppCredentials("../123", &AppCredentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}); err == nil {
		t.Error("stored under an account id that walks out of the store")
	}
	if stored, err := LoadAppCredentials(""); stored != nil || err != nil {
		t.Errorf("an empty account id: got %v, %v", stored, err)
	}
	if err := ForgetAppCredentials(""); err != nil {
		t.Errorf("forgetting an empty account id: %v", err)
	}

	breakCredentialStore(t)
	if err := StoreAppCredentials("123456789012", &AppCredentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}); err == nil {
		t.Error("stored a key with nowhere to put it")
	}
}

func TestLinuxCredentialsReportWhatTheyCannotRead(t *testing.T) {
	isolateCredentialStore(t)
	path, err := credentialPath("123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("no-separator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAppCredentials("123456789012"); err == nil || !strings.Contains(err.Error(), "not a key pair") {
		t.Errorf("got %v", err)
	}

	// A directory where the key should be cannot be removed as a file, and
	// saying it was would leave the uninstall believing it had finished.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ForgetAppCredentials("123456789012"); err == nil {
		t.Error("a key that could not be removed was reported gone")
	}
	if err := StoreAppCredentials("123456789012", &AppCredentials{AccessKeyID: "AKIA", SecretAccessKey: "s"}); err == nil {
		t.Error("a key that could not be put in place was reported stored")
	}

	t.Setenv("HOME", "")
	if _, err := credentialPath("123456789012"); err == nil {
		t.Error("a store with no home")
	}
}
