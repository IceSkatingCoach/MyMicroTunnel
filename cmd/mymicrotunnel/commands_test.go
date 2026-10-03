// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/version"
)

func labProfile() setup.Settings {
	s := setup.Defaults()
	s.ProfileName = "lab"
	s.Profile = "default"
	s.Region = fakeRegion
	s.StackName = "lab-stack"
	s.DomainName = "lab.example.com"
	s.InterfaceName = "wg3"
	s.VpnCidr = "10.231.77.0/24"
	s.AlignAddressesToVpnCidr()
	return s
}

// --- main ---------------------------------------------------------------------

func runMain(t *testing.T, args ...string) outcome {
	t.Helper()
	// main extends PATH for a launchd-started process; t.Setenv puts it back.
	t.Setenv("PATH", os.Getenv("PATH"))
	stub(t, &os.Args, append([]string{"mymicrotunnel"}, args...))
	return invoke(t, func([]string) { main() })
}

func TestMainPrintsTheVersionAndTheUsage(t *testing.T) {
	isolate(t)
	runMain(t, "version").mustSucceed(t).mustPrint(t, version.String())
	runMain(t, "--version").mustSucceed(t).mustPrint(t, version.String())
	runMain(t, "help").mustSucceed(t).mustPrint(t, "install    deploy the stack", "--domain HOST")
}

func TestMainListsTheAWSProfilesForTheSetupWindow(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	out := runMain(t, "profiles").mustSucceed(t).out
	if out != "default\nnoregion\n" {
		t.Errorf("profiles printed %q", out)
	}
}

// Each subcommand reaches its own handler. Stopped at the first step each one
// takes, which is enough to tell them apart.
func TestMainDispatchesEverySubcommand(t *testing.T) {
	isolate(t)
	stub(t, &prerequisites, func(bool) string { fail("prerequisites reached"); return "" })
	stub(t, &superviseProfiles, func(setup.SuperviseOptions) {})
	stub(t, &diagnose, func(context.Context, setup.DiagnoseOptions) string { return "diagnosed\n" })
	stub(t, &uninstall, func(context.Context, setup.UninstallOptions) {})

	for _, tc := range []struct {
		args []string
		fail string
		out  string
	}{
		{args: nil, fail: "prerequisites reached"},
		{args: []string{"install", "--json"}, fail: "prerequisites reached"},
		{args: []string{"--json"}, fail: "prerequisites reached"},
		{args: []string{"uninstall", "--json", "--non-interactive"}},
		{args: []string{"status"}, out: "is down"},
		{args: []string{"peers"}, fail: "No stack"},
		{args: []string{"profile"}, out: "No VPN profile is installed."},
		{args: []string{"wake"}, fail: "No profile called"},
		{args: []string{"supervise", "--once"}},
		{args: []string{"tunnel"}, fail: "Usage: mymicrotunnel tunnel"},
		{args: []string{"diagnose"}, out: "diagnosed"},
		{args: []string{"reload"}, fail: "needs root"},
		{args: []string{"pubkey", filepath.Join(t.TempDir(), "missing")}, fail: "Could not read"},
		{args: []string{"regions", "--profile", "nobody"}},
		{args: []string{"default-stack", "--profile", "nobody"}},
	} {
		// Interactive install banners go to stdout; the prompts are stubbed.
		result := runMain(t, tc.args...)
		if tc.fail != "" {
			result.mustFail(t, tc.fail)
			continue
		}
		result.mustSucceed(t).mustPrint(t, tc.out)
	}
}

// --- pubkey -----------------------------------------------------------------

func TestPubkeyReadsTheKeyFileAndPrintsOnlyItsPublicHalf(t *testing.T) {
	isolate(t)
	private, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := tunnel.PublicKey(private)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "wg0.key")
	if err := os.WriteFile(path, []byte(private+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := invoke(t, runPubkey, path).mustSucceed(t).out
	if out != public+"\n" {
		t.Errorf("pubkey printed %q, want %q", out, public)
	}
	if strings.Contains(out, private) {
		t.Error("pubkey printed the private key")
	}
}

func TestPubkeyRefusesAFileThatIsNotAKey(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	invoke(t, runPubkey, path).mustFail(t, "does not hold a WireGuard key")
}

// --- regions and default-stack ----------------------------------------------

func TestRegionsPrintsWhatTheAccountOffersOnePerLine(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.regions = []string{"us-east-1", "eu-west-1", "ap-southeast-2"}

	out := invoke(t, runRegions).mustSucceed(t).out
	if out != "ap-southeast-2\neu-west-1\nus-east-1\n" {
		t.Errorf("regions printed %q", out)
	}
}

// The setup window has a built-in list; anything printed on failure would be
// read as a region.
func TestRegionsPrintsNothingWhenAWSRefuses(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.denied["DescribeRegions"] = true

	if out := invoke(t, runRegions, "--profile", "noregion").mustSucceed(t).out; out != "" {
		t.Errorf("regions printed %q on failure", out)
	}
	if !fake.called("DescribeRegions") {
		t.Error("a profile without a region did not fall back to us-east-1 and ask")
	}
}

func TestDefaultStackIsNamedAfterTheAccountRegionAndProfile(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)

	out := invoke(t, runDefaultStack).mustSucceed(t).out
	if want := "mymicrotunnel-" + fakeAccount + "-" + fakeRegion + "\n"; out != want {
		t.Errorf("default-stack printed %q, want %q", out, want)
	}
	out = invoke(t, runDefaultStack, "--region", "us-west-2", "--vpn-profile", "lab").mustSucceed(t).out
	if want := "mymicrotunnel-" + fakeAccount + "-us-west-2-lab\n"; out != want {
		t.Errorf("default-stack printed %q, want %q", out, want)
	}
}

// Read by the setup window as a stack name, so silence is the only safe
// failure.
func TestDefaultStackPrintsNothingItCannotWorkOut(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)

	if out := invoke(t, runDefaultStack, "--profile", "noregion").mustSucceed(t).out; out != "" {
		t.Errorf("no region, yet default-stack printed %q", out)
	}
	if out := invoke(t, runDefaultStack, "--profile", "nobody", "--region", "us-east-1").mustSucceed(t).out; out != "" {
		t.Errorf("no such profile, yet default-stack printed %q", out)
	}
	fake.denied["GetCallerIdentity"] = true
	if out := invoke(t, runDefaultStack).mustSucceed(t).out; out != "" {
		t.Errorf("no account id, yet default-stack printed %q", out)
	}
}

// --- peers ------------------------------------------------------------------

func TestPeersNeedsAStackFromTheProfileOrTheCommandLine(t *testing.T) {
	isolate(t)
	invoke(t, runPeers, "--vpn-profile", "lab").mustFail(t, `profile "lab" is not installed here, so pass --stack`)
}

func TestPeersListsTheWorkstationsTheProfileRegistered(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	saveProfile(t, labProfile())
	parameter := setup.Settings{StackName: "lab-stack"}.PeersParameter()
	fake.parameters[parameter] = `[{"publicKey":"KEYA","address":"10.231.77.2","label":"laptop"},` +
		`{"publicKey":"KEYB","address":"10.231.77.3"}]`

	invoke(t, runPeers, "--vpn-profile", "lab").mustSucceed(t).
		mustPrint(t, "10.231.77.2      laptop                   KEYA", "10.231.77.3      (unnamed)                KEYB")
}

func TestPeersSaysSoWhenNoWorkstationIsRegistered(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	invoke(t, runPeers, "--stack", "empty-stack").mustSucceed(t).
		mustPrint(t, "No workstation is registered for empty-stack.")
}

func TestPeersReportsWhatAWSRefused(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.denied["GetParameter"] = true

	invoke(t, runPeers, "--stack", "s").mustFail(t, "Could not read /")
	invoke(t, runPeers, "--stack", "s", "--remove", "10.0.0.2").mustFail(t, "Could not remove 10.0.0.2")
	invoke(t, runPeers, "--stack", "s", "--profile", "nobody").mustFail(t, "Could not load AWS credentials")
}

// Retiring a workstation takes it out of the registry and out of every target
// group, each on the port it was registered with.
func TestPeersRemoveRetiresTheWorkstationEverywhere(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	parameter := setup.Settings{StackName: "s"}.PeersParameter()
	fake.parameters[parameter] = `[{"publicKey":"KEYA","address":"10.0.0.2"},{"publicKey":"KEYB","address":"10.0.0.3"}]`
	fake.outputs["TargetGroupArn"] = "arn:tg/https"
	fake.outputs["TcpTarget5432"] = "5432=arn:tg/postgres"
	fake.outputs["TcpTargetOdd"] = "3000:8080=arn:tg/odd"
	fake.stackParameters["ServicePort"] = "3010"

	invoke(t, runPeers, "--stack", "s", "--remove", "10.0.0.2").mustSucceed(t).
		mustPrint(t, "Removed 10.0.0.2 from s.")

	if got := fake.parameters[parameter]; strings.Contains(got, "10.0.0.2") || !strings.Contains(got, "10.0.0.3") {
		t.Errorf("the registry now holds %s", got)
	}
	want := map[string]bool{"arn:tg/https 10.0.0.2:3010": true, "arn:tg/postgres 10.0.0.2:5432": true}
	if len(fake.deregistered) != len(want) {
		t.Errorf("deregistered %v, want %v", fake.deregistered, want)
	}
	for _, got := range fake.deregistered {
		if !want[got] {
			t.Errorf("deregistered %s unexpectedly", got)
		}
	}
}

func TestPeersRemoveWarnsWhenTheLoadBalancerKeepsTheTarget(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.parameters[setup.Settings{StackName: "s"}.PeersParameter()] = `[{"publicKey":"KEYA","address":"10.0.0.2"}]`
	fake.outputs["TargetGroupArn"] = "arn:tg/https"
	fake.outputs["TcpTarget5432"] = "5432=arn:tg/postgres"
	fake.stackParameters["ServicePort"] = "3000"
	fake.denied["DeregisterTargets"] = true

	invoke(t, runPeers, "--stack", "s", "--remove", "10.0.0.2").mustSucceed(t).
		mustPrint(t, "Removed from the peer list, but not from the load balancer",
			"Still registered on TCP 5432", "Removed 10.0.0.2 from s.")
}

// --- profile ----------------------------------------------------------------

func TestProfileListShowsEveryProfileAndWhichAreUp(t *testing.T) {
	isolate(t)
	saveProfile(t, labProfile())
	other := labProfile()
	other.ProfileName = "work"
	other.InterfaceName = "wg4"
	other.StackName = "work-stack"
	saveProfile(t, other)
	stub(t, &tunnelIsUp, func(name string) bool { return name == "wg3" })

	out := invoke(t, runProfile, "list").mustSucceed(t).out
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "PROFILE") {
		t.Fatalf("profile list printed:\n%s", out)
	}
	if !strings.Contains(lines[1], "lab") || !strings.Contains(lines[1], " up ") || !strings.Contains(lines[1], "lab-stack") {
		t.Errorf("the lab row reads %q", lines[1])
	}
	if !strings.Contains(lines[2], "work") || !strings.Contains(lines[2], " down ") {
		t.Errorf("the work row reads %q", lines[2])
	}
}

func TestProfileShowDescribesTheDeployment(t *testing.T) {
	isolate(t)
	s := labProfile()
	s.TcpPorts = []string{"5432", "3000:8080"}
	s.IdleTimeoutMinutes = 30
	s.Supervise = true
	saveProfile(t, s)

	invoke(t, runProfile, "show", "lab").mustSucceed(t).mustPrint(t,
		"profile        lab",
		"stack          lab-stack in "+fakeRegion,
		"hostname       https://lab.example.com",
		"interface      wg3",
		"exposed TCP    5432, 3000:8080",
		"idle timeout   30 minutes",
		"supervised     true")
}

func TestProfileRefusesWhatItCannotShow(t *testing.T) {
	isolate(t)
	invoke(t, runProfile, "show").mustFail(t, "Usage: mymicrotunnel profile show NAME")
	invoke(t, runProfile, "show", "ghost").mustFail(t, `No profile called "ghost"`)
	invoke(t, runProfile, "rename").mustFail(t, `Unknown profile command "rename"`)
}

// --- profile save -------------------------------------------------------------

func TestProfileSaveRecordsADraftOnAnInterfaceNobodyHolds(t *testing.T) {
	isolate(t)
	saveProfile(t, labProfile())

	out := invoke(t, runProfile, "save", "--vpn-profile", "draft", "--region", "us-east-2",
		"--domain", "draft.example.com", "--port", "8080:8443", "--tcp-ports", "5432,6379:16379",
		"--idle-timeout", "15", "--supervise", "--vpn-cidr", "10.232.0.0/24").mustSucceed(t).out

	path := setup.ProfileSettingsPath("draft")
	if strings.TrimSpace(out) != path {
		t.Errorf("profile save printed %q, want the path %s", out, path)
	}
	saved, err := setup.LoadProfileSettings("draft")
	if err != nil {
		t.Fatal(err)
	}
	if saved.InterfaceName == "wg3" || saved.InterfaceName == "" {
		t.Errorf("the draft claimed interface %q", saved.InterfaceName)
	}
	if saved.ServicePort != "8080" || saved.PublishedPort != "8443" {
		t.Errorf("the port was saved as %s:%s", saved.ServicePort, saved.PublishedPort)
	}
	if strings.Join(saved.TcpPorts, ",") != "5432,6379:16379" {
		t.Errorf("the TCP ports were saved as %v", saved.TcpPorts)
	}
	if saved.IdleTimeoutMinutes != 15 || !saved.Supervise || saved.Region != "us-east-2" {
		t.Errorf("saved %+v", saved)
	}
	if !strings.HasPrefix(saved.ClientAddress, "10.232.0.") {
		t.Errorf("the tunnel address %s did not follow the subnet", saved.ClientAddress)
	}
	if saved.Endpoint != "" {
		t.Error("a saved draft claims a deployment")
	}
}

func TestProfileSaveChangesOnlyWhatWasTyped(t *testing.T) {
	isolate(t)
	s := labProfile()
	s.IdleTimeoutMinutes = 20
	saveProfile(t, s)

	invoke(t, runProfile, "save", "--vpn-profile", "lab", "--stack", "renamed", "--idle-timeout", "-1").mustSucceed(t)

	saved, err := setup.LoadProfileSettings("lab")
	if err != nil {
		t.Fatal(err)
	}
	if saved.StackName != "renamed" {
		t.Errorf("the stack is %q", saved.StackName)
	}
	if saved.DomainName != s.DomainName || saved.InterfaceName != s.InterfaceName || saved.IdleTimeoutMinutes != 20 {
		t.Errorf("saving one field changed others: %+v", saved)
	}
}

func TestProfileSaveRefusesPortsThatDoNotParse(t *testing.T) {
	isolate(t)
	invoke(t, runProfile, "save", "--port", "nope").mustFail(t, `"nope"`)
	invoke(t, runProfile, "save", "--tcp-ports", "1,2,x").mustFail(t, `"x"`)
	if _, err := os.Stat(setup.ProfileSettingsPath("default")); err == nil {
		t.Error("a refused save still wrote the profile")
	}
}

// --- wake -------------------------------------------------------------------

func wakeProfile(t *testing.T) {
	t.Helper()
	s := labProfile()
	s.GatewayGroupName = "lab-gateway"
	saveProfile(t, s)
}

func TestWakeSaysWhenTheGatewayIsAlreadyRunning(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	fake.inService = true
	wakeProfile(t)

	invoke(t, runWake, "--vpn-profile", "lab").mustSucceed(t).mustPrint(t, "the gateway is already running")
	if out := invoke(t, runWake, "--vpn-profile", "lab", "--quiet").mustSucceed(t).out; out != "" {
		t.Errorf("--quiet printed %q", out)
	}
	if fake.called("SetDesiredCapacity") {
		t.Error("a running gateway was asked for another instance")
	}
}

func TestWakeAsksForAnInstanceWhenTheGatewayIsOff(t *testing.T) {
	home := isolate(t)
	fake := newFakeAWS(t, home)
	wakeProfile(t)

	invoke(t, runWake, "--vpn-profile", "lab", "--wait", "0").mustSucceed(t).mustPrint(t, "asked for a gateway instance")
	if fake.desired != 1 {
		t.Errorf("the group's desired capacity is %d", fake.desired)
	}
}

func TestWakeReportsWhyItCouldNot(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	invoke(t, runWake, "--vpn-profile", "ghost").mustFail(t, `No profile called "ghost"`)

	saveProfile(t, labProfile())
	invoke(t, runWake, "--vpn-profile", "lab").mustFail(t, "Could not wake the gateway for lab: no Auto Scaling group")

	s := labProfile()
	s.Profile = "nobody"
	saveProfile(t, s)
	invoke(t, runWake, "--vpn-profile", "lab").mustFail(t, "Could not load AWS credentials")
}

// --- supervise and diagnose ---------------------------------------------------

func TestSuperviseWatchesTheWholeStoreUnlessGivenOneProfile(t *testing.T) {
	isolate(t)
	var got setup.SuperviseOptions
	stub(t, &superviseProfiles, func(options setup.SuperviseOptions) { got = options })

	invoke(t, runSupervise, "--once").mustSucceed(t)
	if got.ProfilesDir != setup.ProfilesDir() || !got.Once || got.StatePath != "" {
		t.Errorf("supervise without arguments ran with %+v", got)
	}

	invoke(t, runSupervise, "--state", "/tmp/state", "--interface", "wg7", "--config", "/tmp/wg7.conf",
		"--wake-user", "alice").mustSucceed(t)
	want := setup.SuperviseOptions{StatePath: "/tmp/state", InterfaceName: "wg7", ConfigPath: "/tmp/wg7.conf", WakeUser: "alice"}
	if got != want {
		t.Errorf("supervise for one profile ran with %+v, want %+v", got, want)
	}
}

func TestDiagnoseFillsInWhatTheProfileRecorded(t *testing.T) {
	isolate(t)
	saveProfile(t, labProfile())
	var got setup.DiagnoseOptions
	stub(t, &diagnose, func(_ context.Context, options setup.DiagnoseOptions) string {
		got = options
		return "the report\n"
	})

	out := invoke(t, runDiagnose, "--vpn-profile", "lab", "--no-aws").mustSucceed(t).out
	if out != "the report\n" {
		t.Errorf("diagnose printed %q", out)
	}
	want := setup.DiagnoseOptions{ProfileName: "lab", StackName: "lab-stack", Profile: "default", Region: fakeRegion, SkipAWS: true}
	if got != want {
		t.Errorf("diagnose ran with %+v, want %+v", got, want)
	}
}

func TestDiagnoseWritesTheReportWhereItIsTold(t *testing.T) {
	isolate(t)
	stub(t, &diagnose, func(context.Context, setup.DiagnoseOptions) string { return "the report\n" })

	path := filepath.Join(t.TempDir(), "report.txt")
	invoke(t, runDiagnose, "-o", path).mustSucceed(t).mustPrint(t, "Written to "+path)
	if got := readFile(t, path); got != "the report\n" {
		t.Errorf("the file holds %q", got)
	}
	invoke(t, runDiagnose, "-o", filepath.Join(t.TempDir(), "missing", "report.txt")).mustFail(t, "Could not write")
}

// --- uninstall --------------------------------------------------------------

func TestUninstallTakesTheStackFromTheProfile(t *testing.T) {
	isolate(t)
	saveProfile(t, labProfile())
	var got setup.UninstallOptions
	stub(t, &uninstall, func(_ context.Context, options setup.UninstallOptions) { got = options })

	out := invoke(t, runUninstall, "--vpn-profile", "lab", "--non-interactive", "--json", "--delete-stack", "--keep-app").
		mustSucceed(t).out
	if strings.Contains(out, "Uninstalled") {
		t.Errorf("--json printed prose: %q", out)
	}
	want := setup.UninstallOptions{ProfileName: "lab", KeepApp: true, DeleteStack: true, StackName: "lab-stack",
		Profile: "default", Region: fakeRegion, NonInteractive: true}
	if got != want {
		t.Errorf("uninstall ran with %+v, want %+v", got, want)
	}
}

func TestUninstallFallsBackToTheAWSProfileRegion(t *testing.T) {
	home := isolate(t)
	newFakeAWS(t, home)
	var got setup.UninstallOptions
	stub(t, &uninstall, func(_ context.Context, options setup.UninstallOptions) { got = options })

	invoke(t, runUninstall, "--non-interactive", "--json").mustSucceed(t)
	if got.Region != fakeRegion || got.Profile != "default" {
		t.Errorf("uninstall ran with %+v", got)
	}
}

func TestUninstallAsksBeforeDeletingAnything(t *testing.T) {
	isolate(t)
	saveProfile(t, labProfile())
	var got setup.UninstallOptions
	stub(t, &uninstall, func(_ context.Context, options setup.UninstallOptions) { got = options })
	converse(t, map[string]string{
		"Also delete /etc/wireguard":   "y",
		"Also delete the AWS":          "y",
		"Stack name":                   "typed-stack",
		"AWS profile":                  "",
		"Region":                       "us-west-1",
		"This takes the hostname down": "n",
	})

	invoke(t, runUninstall, "--vpn-profile", "lab").mustSucceed(t).
		mustPrint(t, "MyMicroTunnel uninstaller", "✓ Uninstalled.")
	if !got.DeleteKeys || got.DeleteStack {
		t.Errorf("declining the final question still deleted the stack: %+v", got)
	}
	if got.StackName != "typed-stack" || got.Profile != "default" || got.Region != "us-west-1" {
		t.Errorf("the answers were not used: %+v", got)
	}
}

func TestUninstallDeletesTheStackOnceConfirmed(t *testing.T) {
	isolate(t)
	saveProfile(t, labProfile())
	var got setup.UninstallOptions
	stub(t, &uninstall, func(_ context.Context, options setup.UninstallOptions) { got = options })
	converse(t, map[string]string{
		"Stack name":                   "",
		"AWS profile":                  "",
		"Region":                       "",
		"This takes the hostname down": "y",
	})

	invoke(t, runUninstall, "--vpn-profile", "lab", "--delete-stack", "--delete-keys").mustSucceed(t)
	if !got.DeleteStack || !got.DeleteKeys || got.StackName != "lab-stack" {
		t.Errorf("uninstall ran with %+v", got)
	}
}

// --- reload -----------------------------------------------------------------

func TestReloadNeedsRoot(t *testing.T) {
	isolate(t)
	invoke(t, runReload).mustFail(t, "Reloading the tunnels needs root")
}

// Only tunnels that are up are bounced, and one that will not come back does
// not stop the others.
func TestReloadReRaisesEveryTunnelThatIsUp(t *testing.T) {
	isolate(t)
	stub(t, &geteuid, func() int { return 0 })
	for _, profile := range []struct{ name, iface string }{
		{"alpha", "wg1"}, {"beta", "wg2"}, {"gamma", "wg3"}, {"delta", "wg4"},
	} {
		s := labProfile()
		s.ProfileName = profile.name
		s.InterfaceName = profile.iface
		saveProfile(t, s)
	}
	stub(t, &tunnelIsUp, func(name string) bool { return name != "wg4" })
	var raised []string
	stub(t, &tunnelDown, func(name string) error {
		if name == "wg2" {
			return errors.New("busy")
		}
		return nil
	})
	stub(t, &raiseTunnel, func(name, config string) error {
		raised = append(raised, name)
		if name == "wg3" {
			return errors.New("no config")
		}
		return nil
	})

	out := invoke(t, runReload).mustSucceed(t).mustPrint(t,
		"re-raising wg1 (alpha)", "could not drop wg2: busy", "could not raise wg3: no config").out
	if strings.Contains(out, "wg4") {
		t.Errorf("a tunnel that was down was touched:\n%s", out)
	}
	if strings.Join(raised, ",") != "wg1,wg3" {
		t.Errorf("raised %v", raised)
	}
}

// --- tunnel and status ------------------------------------------------------

func TestTunnelUpAndDownNeedRoot(t *testing.T) {
	isolate(t)
	invoke(t, runTunnel).mustFail(t, "Usage: mymicrotunnel tunnel up|down|status")
	invoke(t, runTunnel, "up").mustFail(t, "Raising the tunnel needs root.")
	invoke(t, runTunnel, "down").mustFail(t, "Dropping the tunnel needs root.")
	invoke(t, runTunnel, "sideways").mustFail(t, `Unknown tunnel command "sideways"`)
}

func TestTunnelUpRaisesTheNamedInterface(t *testing.T) {
	isolate(t)
	stub(t, &geteuid, func() int { return 0 })
	var gotName, gotConfig string
	stub(t, &raiseTunnel, func(name, config string) error {
		gotName, gotConfig = name, config
		return nil
	})
	stub(t, &tunnelDevice, func(string) string { return "utun9" })

	invoke(t, runTunnel, "up", "wg5", "--config", "/tmp/try.conf").mustSucceed(t).mustPrint(t, "wg5 is up on utun9")
	if gotName != "wg5" || gotConfig != "/tmp/try.conf" {
		t.Errorf("raised %s with %q", gotName, gotConfig)
	}

	stub(t, &raiseTunnel, func(string, string) error { return errors.New("overlaps en0") })
	invoke(t, runTunnel, "up").mustFail(t, "overlaps en0")
}

func TestTunnelDownDropsTheInterface(t *testing.T) {
	isolate(t)
	stub(t, &geteuid, func() int { return 0 })
	var dropped string
	stub(t, &tunnelDown, func(name string) error { dropped = name; return nil })

	invoke(t, runTunnel, "down").mustSucceed(t).mustPrint(t, "wg0 is down")
	if dropped != "wg0" {
		t.Errorf("dropped %q, want the default interface", dropped)
	}
	stub(t, &tunnelDown, func(string) error { return errors.New("no such device") })
	invoke(t, runTunnel, "down", "wg2").mustFail(t, "no such device")
}

func TestTunnelStatusSaysDownWhenNothingIsThere(t *testing.T) {
	isolate(t)
	invoke(t, runTunnel, "status", "wg6").mustSucceed(t).mustPrint(t, "wg6 is down")
}

// Unprivileged, an up tunnel is only visible as its address; that must not
// be reported as down.
func TestStatusWithoutRootReportsTheTunnelAsUpButUnreadable(t *testing.T) {
	isolate(t)
	stub(t, &tunnelDevice, func(string) string { return "utun4" })
	stub(t, &tunnelReport, func(string) (tunnel.Status, error) { return tunnel.Status{}, errors.New("permission denied") })
	stub(t, &tunnelAddressPresent, func(address string) bool { return address == "10.9.0.2" })

	reportOut := invoke(t, func([]string) { reportTunnel("wg1", "10.9.0.2") }).mustSucceed(t)
	reportOut.mustPrint(t, "wg1 is up (10.9.0.2), but reading its handshake needs root:", "tunnel status wg1")
}

func TestStatusAsRootFailsWhenTheInterfaceCannotBeRead(t *testing.T) {
	isolate(t)
	stub(t, &geteuid, func() int { return 0 })
	stub(t, &tunnelDevice, func(string) string { return "utun4" })
	stub(t, &tunnelReport, func(string) (tunnel.Status, error) { return tunnel.Status{}, errors.New("uapi closed") })

	invoke(t, func([]string) { reportTunnel("wg1", "") }).mustFail(t, "uapi closed")
}

func TestStatusListsThePeersAndWhetherAnyIsAnswering(t *testing.T) {
	isolate(t)
	stub(t, &tunnelDevice, func(string) string { return "utun4" })
	recent := tunnel.Status{Peers: []tunnel.PeerStatus{
		{PublicKey: "KEYA", Endpoint: "203.0.113.1:51820", LastHandshake: time.Now().Add(-5 * time.Second), ReceivedBytes: 10, SentBytes: 20},
		{PublicKey: "KEYB", Endpoint: "203.0.113.2:51820"},
	}}
	stub(t, &tunnelReport, func(string) (tunnel.Status, error) { return recent, nil })

	out := invoke(t, func([]string) { reportTunnel("wg1", "") }).mustSucceed(t).
		mustPrint(t, "wg1 is up on utun4", "peer KEYA via 203.0.113.1:51820, last handshake", "ago, rx 10 tx 20",
			"peer KEYB via 203.0.113.2:51820, last handshake never").out
	if strings.Contains(out, "carries nothing") {
		t.Errorf("a peer that handshook seconds ago was reported silent:\n%s", out)
	}

	silent := tunnel.Status{Peers: recent.Peers[1:]}
	stub(t, &tunnelReport, func(string) (tunnel.Status, error) { return silent, nil })
	invoke(t, func([]string) { reportTunnel("wg1", "") }).mustSucceed(t).
		mustPrint(t, "no peer has handshaken recently")
}

// The installed profile's interface and address, not wg0's: a second
// deployment reported against the defaults reads "down" while it is up.
func TestStatusReportsTheProfilesOwnInterface(t *testing.T) {
	isolate(t)
	s := labProfile()
	if err := setup.WriteAppConfig(s); err != nil {
		t.Fatal(err)
	}
	stub(t, &tunnelAddressPresent, func(address string) bool { return address == s.ClientAddress })

	invoke(t, runStatus, "--vpn-profile", "lab").mustSucceed(t).
		mustPrint(t, "wg3 is up ("+s.ClientAddress+")")

	out := invoke(t, runStatus, "--vpn-profile", "lab", "--json").mustSucceed(t).out
	for _, want := range []string{`"kind":"result"`, `"up":"true"`, `"connected":"false"`, `"interface":"wg3"`,
		`"profile":"lab"`, `"version":"` + version.String() + `"`} {
		if !strings.Contains(out, want) {
			t.Errorf("status --json lacks %s:\n%s", want, out)
		}
	}
}
