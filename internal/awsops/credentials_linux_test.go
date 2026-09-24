// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"os"
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
