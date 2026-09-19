// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAwsFile(t *testing.T, home, name, content string) {
	t.Helper()
	directory := filepath.Join(home, ".aws")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProfilesReadsBothSharedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeAwsFile(t, home, "credentials", "[default]\naws_access_key_id = A\n\n[work]\naws_access_key_id = B\n")
	// ~/.aws/config spells them "profile NAME", except for "default".
	writeAwsFile(t, home, "config", "[default]\nregion = us-east-1\n\n[profile sso-admin]\nregion = eu-west-1\n")

	names := strings.Join(Profiles(), ",")
	if names != "default,sso-admin,work" {
		t.Errorf("got %q, want %q", names, "default,sso-admin,work")
	}
}

func TestProfilesWithNothingConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := Profiles(); len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

func TestWriteProfileLeavesEveryOtherSectionAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeAwsFile(t, home, "credentials",
		"[default]\naws_access_key_id = KEEP\naws_secret_access_key = KEEPSECRET\n\n"+
			"[other]\naws_access_key_id = ALSOKEEP\n")

	if err := WriteProfile("mymicrotunnel", "NEWKEY", "NEWSECRET", "us-east-2"); err != nil {
		t.Fatalf("writing the profile: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(home, ".aws", "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	written := string(content)

	// The installer writes into a file the user already keeps their working
	// life in. Losing a section here costs them more than this tool is worth.
	for _, expected := range []string{"KEEP", "KEEPSECRET", "ALSOKEEP", "[other]", "[default]"} {
		if !strings.Contains(written, expected) {
			t.Errorf("%q did not survive the write:\n%s", expected, written)
		}
	}
	for _, expected := range []string{"[mymicrotunnel]", "NEWKEY", "NEWSECRET", "region = us-east-2"} {
		if !strings.Contains(written, expected) {
			t.Errorf("the new profile has no %q:\n%s", expected, written)
		}
	}
}

func TestWriteProfileReplacesRatherThanDuplicates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteProfile("mymicrotunnel", "FIRST", "FIRSTSECRET", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfile("mymicrotunnel", "SECOND", "SECONDSECRET", "us-east-2"); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(filepath.Join(home, ".aws", "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	written := string(content)

	if strings.Count(written, "[mymicrotunnel]") != 1 {
		t.Errorf("the profile appears %d times:\n%s", strings.Count(written, "[mymicrotunnel]"), written)
	}
	// A stale key left above the new one is the one the SDK reads.
	if strings.Contains(written, "FIRSTSECRET") {
		t.Errorf("the replaced credentials are still in the file:\n%s", written)
	}
	if !strings.Contains(written, "SECONDSECRET") {
		t.Errorf("the new credentials are missing:\n%s", written)
	}
}

func TestWriteProfileKeepsTheFilePrivate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := WriteProfile("mymicrotunnel", "KEY", "SECRET", "us-east-1"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(home, ".aws", "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the credentials file is mode %o, want 600", mode)
	}
}
