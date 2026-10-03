// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"errors"
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

// isolatedAWSConfig points every place the SDK reads configuration from at
// an empty home, so the developer's own profiles never leak into a test.
func isolatedAWSConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, ".aws", "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, ".aws", "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, name := range []string{
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	} {
		t.Setenv(name, "")
	}
	return home
}

func TestLoadProfileUsesTheProfilesKeyAndTheRegionAskedFor(t *testing.T) {
	home := isolatedAWSConfig(t)
	writeAwsFile(t, home, "credentials", "[work]\naws_access_key_id = AKIAWORK\naws_secret_access_key = worksecret\n")
	writeAwsFile(t, home, "config", "[profile work]\nregion = us-west-2\n")

	client, err := LoadProfile(context.Background(), "work", "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	if client.Region != "eu-central-1" {
		t.Errorf("region %q, want eu-central-1", client.Region)
	}
	if credentials, err := client.cfg.Credentials.Retrieve(context.Background()); err != nil || credentials.AccessKeyID != "AKIAWORK" {
		t.Errorf("got %v, %v", credentials.AccessKeyID, err)
	}
	if client.CFN == nil || client.SSM == nil || client.ELB == nil || client.STS == nil || client.EC2 == nil ||
		client.Route53 == nil || client.S3 == nil || client.CloudFront == nil {
		t.Errorf("a service client is missing: %+v", client)
	}
	// Route53 and CloudFront are global; signing them for eu-central-1 fails.
	if region := client.Route53.Options().Region; region != "us-east-1" {
		t.Errorf("Route53 region %q", region)
	}
	if region := client.CloudFront.Options().Region; region != "us-east-1" {
		t.Errorf("CloudFront region %q", region)
	}

	if _, err := LoadProfile(context.Background(), "nobody", "eu-central-1"); err == nil {
		t.Error("a profile that does not exist was loaded")
	}
}

func TestLoadStaticUsesTheKeyGiven(t *testing.T) {
	isolatedAWSConfig(t)

	client, err := LoadStatic(context.Background(), "AKIASTATIC", "staticsecret", "ap-southeast-2")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := client.cfg.Credentials.Retrieve(context.Background())
	if err != nil || credentials.AccessKeyID != "AKIASTATIC" || credentials.SecretAccessKey != "staticsecret" {
		t.Errorf("got %+v, %v", credentials, err)
	}
	if client.Region != "ap-southeast-2" {
		t.Errorf("region %q", client.Region)
	}

	// The environment can still name a profile; one that does not exist is
	// an error rather than silently ignored.
	t.Setenv("AWS_PROFILE", "nobody")
	if _, err := LoadStatic(context.Background(), "AKIASTATIC", "staticsecret", "ap-southeast-2"); err == nil {
		t.Error("a broken shared configuration went unreported")
	}
}

func TestProfileRegionReadsWhatTheProfileNames(t *testing.T) {
	home := isolatedAWSConfig(t)
	writeAwsFile(t, home, "config", "[profile work]\nregion = us-west-2\n\n[profile bare]\noutput = json\n")

	if region := ProfileRegion(context.Background(), "work"); region != "us-west-2" {
		t.Errorf("got %q", region)
	}
	if region := ProfileRegion(context.Background(), "bare"); region != "" {
		t.Errorf("a profile with no region: got %q", region)
	}
	if region := ProfileRegion(context.Background(), "nobody"); region != "" {
		t.Errorf("a missing profile: got %q", region)
	}
}

func TestIsSSOProfileRecognisesBothSpellings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if IsSSOProfile("legacy") {
		t.Error("no config file at all, and still SSO")
	}

	writeAwsFile(t, home, "config", "[default]\nsso_start_url = https://d.awsapps.com/start\n\n"+
		"[profile legacy]\nregion = us-east-1\nsso_start_url = https://d.awsapps.com/start\n\n"+
		"[profile modern]\nsso-session = corp\n\n"+
		"[profile keys]\nregion = us-east-1\n\n"+
		"[sso-session corp]\nsso_region = us-east-1\n")

	for profile, want := range map[string]bool{"default": true, "legacy": true, "modern": true, "keys": false, "nobody": false} {
		if got := IsSSOProfile(profile); got != want {
			t.Errorf("IsSSOProfile(%q) = %v, want %v", profile, got, want)
		}
	}
}

func TestExplainCredentialFailureSaysWhatToDoNext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAwsFile(t, home, "config", "[profile sso]\nsso-session = corp\n\n[profile keys]\nregion = us-east-1\n")

	cases := []struct {
		profile string
		err     string
		want    []string
	}{
		{"sso", "operation error STS: InvalidGrantException", []string{"session for \"sso\" has expired", "aws sso login --profile sso"}},
		{"sso", "connection refused", []string{"\"sso\" is an IAM Identity Center profile and it did not work: connection refused", "aws sso login --profile sso"}},
		{"keys", "ExpiredToken: the token has expired", []string{"the credentials for \"keys\" have expired: ExpiredToken"}},
		{"keys", "InvalidClientTokenId", []string{"the credentials for \"keys\" do not work: InvalidClientTokenId", "still active in IAM"}},
	}
	for _, test := range cases {
		message := ExplainCredentialFailure(test.profile, errors.New(test.err))
		for _, want := range test.want {
			if !strings.Contains(message, want) {
				t.Errorf("%s / %s: %q is missing from:\n%s", test.profile, test.err, want, message)
			}
		}
	}
}

func TestProfileFilesNeedAHome(t *testing.T) {
	t.Setenv("HOME", "")

	if names := Profiles(); names != nil {
		t.Errorf("got %v", names)
	}
	if IsSSOProfile("default") {
		t.Error("no home, and still SSO")
	}
	if err := WriteProfile("p", "A", "S", "us-east-1"); err == nil {
		t.Error("wrote a profile with nowhere to write it")
	}
}

func TestWriteProfileReportsAnUnwritableAwsDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".aws"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfile("p", "A", "S", "us-east-1"); err == nil {
		t.Error("~/.aws is a file, and the write still succeeded")
	}

	home = t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".aws", "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Unreadable is not the same as absent: starting from empty would throw
	// away every profile the user had.
	if err := WriteProfile("p", "A", "S", "us-east-1"); err == nil {
		t.Error("an unreadable credentials file was replaced")
	}
}
