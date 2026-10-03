// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"
)

// The install, run against the fake AWS with the steps that would deploy a
// stack, write root-owned files or start the app replaced by recorders. What
// is left is what this file decides: which settings a stage acts on, which
// credentials it uses, and what it records afterwards.

// pretendToDeploy stands in for the AWS-heavy steps and records what they
// were handed.
type pretendToDeploy struct {
	registeredKey string
	rootOwner     string
	rootAsRoot    bool
	rootWrites    int
	loginItems    int
	verified      int
	opened        int
	installed     int
}

func pretend(t *testing.T) *pretendToDeploy {
	t.Helper()
	p := &pretendToDeploy{}
	stub(t, &prerequisites, func(bool) string { return "" })
	stub(t, &discover, func(_ context.Context, _ *awsops.Client, s *setup.Settings) {
		s.VpcID = "vpc-1"
		s.SubnetIDs = []string{"subnet-a", "subnet-b"}
	})
	stub(t, &ensureClientKey, func(*setup.Settings, bool) string { return "GENERATEDKEY" })
	stub(t, &deploy, func(_ context.Context, _ *awsops.Client, s *setup.Settings) {
		s.Endpoint = "203.0.113.9:51820"
		s.ServerPublicKey = "SERVERKEY"
	})
	stub(t, &registerWorkstation, func(_ context.Context, _ *awsops.Client, _ setup.Settings, key string) {
		p.registeredKey = key
	})
	stub(t, &writeRootFiles, func(_ setup.Settings, owner string, asRoot bool) error {
		p.rootOwner, p.rootAsRoot = owner, asRoot
		p.rootWrites++
		return nil
	})
	stub(t, &installApp, func(string) { p.installed++ })
	stub(t, &registerLoginItem, func() { p.loginItems++ })
	stub(t, &verify, func(context.Context, *awsops.Client, setup.Settings) { p.verified++ })
	stub(t, &openApp, func() { p.opened++ })
	return p
}

// result finds the one structured result in NDJSON output.
func result(t *testing.T, out string) map[string]string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		var event struct {
			Kind string            `json:"kind"`
			Data map[string]string `json:"data"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Kind == "result" {
			return event.Data
		}
	}
	t.Fatalf("no result event in:\n%s", out)
	return nil
}

func TestTheDeployStageRecordsWhatItDeployed(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	p := pretend(t)
	// Another profile already holds wg3 and has a deployment, so a new
	// profile typed onto wg3 has to be moved off it.
	saveProfile(t, labProfile())
	minted := 0
	stub(t, &ensureAppCredentials, func(context.Context, *awsops.Client, string) (*awsops.AppCredentials, error) {
		minted++
		return &awsops.AppCredentials{AccessKeyID: "AKIDAPP", SecretAccessKey: "s"}, nil
	})

	carrier := filepath.Join(t.TempDir(), "settings.json")
	out := invoke(t, runInstall, "--json", "--non-interactive", "--stage", "deploy", "--settings", carrier,
		"--vpn-profile", "work", "--profile", "default", "--domain", "svc.example.com",
		"--interface", "wg3", "--port", "3010:8443", "--tcp-ports", "5432,6379:16379", "--idle-timeout", "30",
		"--vpn-cidr", "10.231.88.0/24", "--alarm-on-tunnel-down", "--supervise", "--alarm-email", "ops@example.com",
		"--client-public-key", "TYPEDKEY").mustSucceed(t).out

	data := result(t, out)
	wantStack := "mymicrotunnel-" + fakeAccount + "-" + fakeRegion + "-work"
	if data["stackName"] != wantStack || data["region"] != fakeRegion || data["vpnProfile"] != "work" {
		t.Errorf("the result names %+v", data)
	}
	if data["serviceUrl"] != "https://svc.example.com:8443" || data["endpoint"] != "203.0.113.9:51820" {
		t.Errorf("the result describes %+v", data)
	}
	if data["interface"] == "wg3" || data["interface"] == "" {
		t.Errorf("the new profile was left on interface %q", data["interface"])
	}
	if p.registeredKey != "TYPEDKEY" {
		t.Errorf("registered key %q, not the one typed", p.registeredKey)
	}
	if p.rootWrites != 0 || p.verified != 0 {
		t.Error("the deploy stage went on to the stages after it")
	}
	if minted != 1 || !strings.Contains(out, "Created "+awsops.AppUserName) {
		t.Errorf("the application's own credentials were not minted once:\n%s", out)
	}

	for _, path := range []string{carrier, setup.ProfileSettingsPath("work")} {
		saved, err := setup.ReadSettings(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if saved.StackName != wantStack || saved.AccountID != fakeAccount || saved.ServicePort != "3010" ||
			saved.PublishedPort != "8443" || strings.Join(saved.TcpPorts, ",") != "5432,6379:16379" ||
			saved.IdleTimeoutMinutes != 30 || !saved.Supervise || !saved.AlarmOnTunnelDown ||
			saved.AlarmEmail != "ops@example.com" || saved.AppPrincipalArn != "arn:aws:iam::"+fakeAccount+":user/alice" {
			t.Errorf("%s holds %+v", path, saved)
		}
	}
}

func TestAWholeInstallAsksThenWritesEverything(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	p := pretend(t)
	converse(t, map[string]string{
		"Public hostname": "svc.example.com",
		"Proceed":         "y",
		"Open the app":    "y",
	})
	// Stored once the account is known: the second credential lookup, in the
	// finishing half, authenticates with it instead of the person's profile.
	stub(t, &loadAppCredentials, func(account string) (*awsops.AppCredentials, error) {
		if account != fakeAccount {
			t.Errorf("credentials looked up for account %q", account)
		}
		return &awsops.AppCredentials{AccessKeyID: "AKIDAPP", SecretAccessKey: "s"}, nil
	})

	out := invoke(t, runInstall, "--vpn-profile", "home", "--profile", "default",
		"--vpn-cidr", "10.231.89.0/24", "--tcp-ports", "5432", "--idle-timeout", "10").mustSucceed(t).out

	stack := "mymicrotunnel-" + fakeAccount + "-" + fakeRegion + "-home"
	for _, want := range []string{
		"MyMicroTunnel installer", "About to deploy:", "stack     " + stack + " in " + fakeRegion,
		"hostname  https://svc.example.com", "network   vpc-1, subnets subnet-a, subnet-b",
		"also TCP  5432", "idle      off after 10 minutes", "Using the application's own AWS credentials",
		"from the login Keychain", "✓ Installed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the install did not say %q:\n%s", want, out)
		}
	}
	if setup.HasApp {
		if !strings.Contains(out, "The padlock shield in the menu bar toggles svc.example.com") || p.loginItems != 1 {
			t.Errorf("the app was not offered at login:\n%s", out)
		}
	} else if !strings.Contains(out, "tunnel up|down wg0 toggles svc.example.com") {
		t.Errorf("the closing line does not say how to toggle the tunnel:\n%s", out)
	}
	if p.registeredKey != "GENERATEDKEY" || p.rootWrites != 1 || p.rootAsRoot || p.verified != 1 || p.opened != 1 || p.installed != 1 {
		t.Errorf("the install skipped a step: %+v", p)
	}
	if got := readFile(t, setup.DesiredStatePathFor("home")); got != "up\n" {
		t.Errorf("the desired state is %q, want up", got)
	}
	if _, err := os.Stat(setup.ProfileConfigPath("home")); err != nil {
		t.Errorf("no app configuration was written: %v", err)
	}
}

func TestAnInstallStopsWhenTheUserDeclines(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	p := pretend(t)
	converse(t, map[string]string{"Proceed": "n"})

	invoke(t, runInstall, "--profile", "default", "--domain", "svc.example.com",
		"--vpn-cidr", "10.231.89.0/24").mustFail(t, "Cancelled.")
	if p.registeredKey != "" {
		t.Error("a cancelled install registered a workstation")
	}
}

// The finishing stage applies the carrier file. A stored application key that
// AWS no longer accepts is reported and passed over, not fatal.
func TestTheFinishStageAppliesTheCarrierFile(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.deniedKey = "AKIDREVOKED"
	p := pretend(t)
	stub(t, &loadAppCredentials, func(string) (*awsops.AppCredentials, error) {
		return &awsops.AppCredentials{AccessKeyID: "AKIDREVOKED", SecretAccessKey: "s"}, nil
	})

	staged := labProfile()
	staged.AccountID = fakeAccount
	staged.Endpoint = "203.0.113.9:51820"
	carrier := filepath.Join(t.TempDir(), "settings.json")
	if err := staged.Write(carrier); err != nil {
		t.Fatal(err)
	}

	out := invoke(t, runInstall, "--json", "--non-interactive", "--stage", "finish", "--settings", carrier,
		"--login-item").mustSucceed(t).out
	for _, want := range []string{"The stored application credentials did not work; falling back to default",
		`"kind":"done","message":"Installed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the finish stage did not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ Installed.") {
		t.Error("--json printed prose")
	}
	if p.rootWrites != 0 || p.registeredKey != "" {
		t.Error("the finish stage repeated the deployment")
	}
	if (p.loginItems == 1) != setup.HasApp || p.verified != 1 {
		t.Errorf("the finish stage ran %+v", p)
	}
	config := readFile(t, setup.ProfileConfigPath("lab"))
	if !strings.Contains(config, `"interfaceName": "wg3"`) {
		t.Errorf("the app configuration is not the carrier file's:\n%s", config)
	}
}

func TestTheRootStageWritesOnlyTheRootOwnedFiles(t *testing.T) {
	isolate(t)
	p := pretend(t)
	carrier := filepath.Join(t.TempDir(), "settings.json")
	if err := labProfile().Write(carrier); err != nil {
		t.Fatal(err)
	}

	invoke(t, runInstall, "--stage", "root", "--settings", carrier).mustFail(t, "The root stage must run as root.")

	stub(t, &geteuid, func() int { return 0 })
	t.Setenv("SUDO_USER", "alice")
	invoke(t, runInstall, "--stage", "root", "--settings", carrier, "--json").mustSucceed(t).
		mustPrint(t, "Tunnel configuration and sudoers rule written")
	if p.rootOwner != "alice" || !p.rootAsRoot {
		t.Errorf("the root files were written for %q (as root: %t)", p.rootOwner, p.rootAsRoot)
	}

	stub(t, &writeRootFiles, func(setup.Settings, string, bool) error { return errors.New("read-only file system") })
	invoke(t, runInstall, "--stage", "root", "--settings", carrier).mustFail(t, "read-only file system")
}

func TestAnInstallRefusesWhatCannotBeDeployed(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	pretend(t)
	saveProfile(t, labProfile())

	invoke(t, runInstall, "--non-interactive", "--port", "seventy").mustFail(t, `"seventy"`)
	invoke(t, runInstall, "--non-interactive", "--tcp-ports", "5432,x").mustFail(t, `"x"`)
	invoke(t, runInstall, "--non-interactive", "--stage", "finish", "--settings",
		filepath.Join(t.TempDir(), "missing.json")).mustFail(t, "Cannot read")
	invoke(t, runInstall, "--non-interactive", "--json", "--profile", "default").mustFail(t, "no hostname")
	invoke(t, runInstall, "--non-interactive", "--json", "--profile", "default", "--domain", "svc.example.com",
		"--vpn-profile", "work", "--vpn-cidr", "10.231.77.0/24").mustFail(t, "collides with another one")
}

// --- resolveClient ----------------------------------------------------------

func resolve(t *testing.T, s *setup.Settings, interactive bool, keyID, secret string) outcome {
	t.Helper()
	return invoke(t, func([]string) {
		if client := resolveClient(context.Background(), s, interactive, keyID, secret); client == nil {
			t.Error("resolveClient returned no client")
		}
	})
}

func TestTypedKeysAreSavedAsAProfileAndUsed(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	converse(t, map[string]string{"Region": "us-east-2"})

	s := setup.Defaults()
	resolve(t, &s, true, "AKIDTYPED", "typed-secret").mustSucceed(t).
		mustPrint(t, "Profile mymicrotunnel written to ~/.aws/credentials", "Authenticated as arn:aws:iam::")

	credentials := readFile(t, filepath.Join(home, ".aws", "credentials"))
	if !strings.Contains(credentials, "[mymicrotunnel]\naws_access_key_id = AKIDTYPED") {
		t.Errorf("the typed key was not saved:\n%s", credentials)
	}
	if !strings.Contains(credentials, "[default]") {
		t.Errorf("saving a profile lost the others:\n%s", credentials)
	}
	if s.Region != "us-east-2" || s.StackName != "mymicrotunnel-"+fakeAccount+"-us-east-2" {
		t.Errorf("resolved to %s in %s", s.StackName, s.Region)
	}
}

func TestANonInteractiveRunNeedsSomeCredential(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	s := setup.Defaults()
	resolve(t, &s, false, "", "").mustFail(t, "No AWS profile given")
}

func TestAMachineWithNoProfileIsAskedForAKey(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	for _, name := range []string{"credentials", "config"} {
		if err := os.Remove(filepath.Join(home, ".aws", name)); err != nil {
			t.Fatal(err)
		}
	}
	converse(t, map[string]string{
		"Name for the new profile": "",
		"AWS access key id":        "AKIDASKED",
		"AWS secret access key":    "asked-secret",
		"Region":                   "ca-central-1",
	})

	s := setup.Defaults()
	resolve(t, &s, true, "", "").mustSucceed(t).mustPrint(t, "Profile mymicrotunnel written")
	if credentials := readFile(t, filepath.Join(home, ".aws", "credentials")); !strings.Contains(credentials, "AKIDASKED") {
		t.Errorf("the key typed at the prompt was not saved:\n%s", credentials)
	}
	if s.Profile != "mymicrotunnel" || s.Region != "ca-central-1" {
		t.Errorf("resolved profile %q in %q", s.Profile, s.Region)
	}
}

func TestAnEmptyKeyAtThePromptIsRefused(t *testing.T) {
	isolate(t)
	converse(t, map[string]string{"Name for the new profile": "", "AWS access key id": ""})
	s := setup.Defaults()
	resolve(t, &s, true, "", "").mustFail(t, "An access key id is required.")

	converse(t, map[string]string{"Name for the new profile": "", "AWS access key id": "AKID", "AWS secret access key": ""})
	s = setup.Defaults()
	resolve(t, &s, true, "", "").mustFail(t, "A secret access key is required.")
}

func TestAnExistingProfileIsOfferedAndItsRegionUsed(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	converse(t, map[string]string{"Profile": "default"})

	s := setup.Defaults()
	s.StackName = "chosen"
	resolve(t, &s, true, "", "").mustSucceed(t).
		mustPrint(t, "Existing profiles: default, noregion", "Using the region "+fakeRegion+" from profile default")
	if s.Profile != "default" || s.Region != fakeRegion || s.StackName != "chosen" {
		t.Errorf("resolved %+v", s)
	}
}

func TestAProfileWithoutARegionIsAskedForOne(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	converse(t, map[string]string{"Region": "us-west-2"})

	s := setup.Defaults()
	s.Profile = "noregion"
	resolve(t, &s, true, "", "").mustSucceed(t)
	if s.Region != "us-west-2" {
		t.Errorf("the region is %q", s.Region)
	}
}

func TestCredentialsAWSRefusesStopTheRun(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.denied["GetCallerIdentity"] = true

	s := setup.Defaults()
	s.Profile = "default"
	resolve(t, &s, false, "", "").mustFail(t, `the credentials for "default" do not work`)

	s.Profile = "nobody"
	s.Region = fakeRegion
	resolve(t, &s, false, "", "").mustFail(t, "Could not load AWS credentials")
}

// The account id names the default stack; without it there is nothing to
// call the stack unless the caller already named it.
func TestAMissingAccountIdNeedsAStackName(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.identityLimit = 1

	s := setup.Defaults()
	s.Profile = "default"
	resolve(t, &s, false, "", "").mustFail(t, "Could not read the account id")

	fake.identities = 0
	s = setup.Defaults()
	s.Profile = "default"
	s.StackName = "named"
	resolve(t, &s, false, "", "").mustSucceed(t)
	if s.StackName != "named" || s.AccountID != "" {
		t.Errorf("resolved %s for account %q", s.StackName, s.AccountID)
	}
}

func TestAFailedMintCarriesOnWithThePersonsCredentials(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.arn = "arn:aws:sts::" + fakeAccount + ":assumed-role/Admin/alice@example.com"
	stub(t, &ensureAppCredentials, func(context.Context, *awsops.Client, string) (*awsops.AppCredentials, error) {
		return nil, errors.New("iam:CreateUser denied")
	})

	s := setup.Defaults()
	s.Profile = "default"
	resolve(t, &s, false, "", "").mustSucceed(t).
		mustPrint(t, "Could not create the application's own AWS credentials: iam:CreateUser denied",
			"Carrying on with default")
	if strings.Contains(s.AppPrincipalArn, "alice@example.com") {
		t.Errorf("the wake role would trust a session that expires: %s", s.AppPrincipalArn)
	}
}
