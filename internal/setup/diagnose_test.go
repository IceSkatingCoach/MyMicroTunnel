// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
)

func reportOf(fill func(*Report)) string {
	report := &Report{}
	fill(report)
	return report.String()
}

func diagnosedConfig() appConfig {
	return appConfig{
		ProfileName:    DefaultProfileName,
		InterfaceName:  "wg0",
		ClientAddress:  "10.100.0.2",
		ServicePort:    "3000",
		HealthCheckURL: "https://updates.example.com/hc",
		HelperPath:     HelperPath,
	}
}

// It is pasted into email: the whole thing has to come out, in order, whatever
// is broken.
func TestADiagnosisCoversEverySectionInOrder(t *testing.T) {
	fake := stubMachine(t)
	s := valid()
	s.Supervise = true
	s.TcpPorts = []string{"5432"}
	s.IdleTimeoutMinutes = 30
	s.WakeRoleARN = "arn:role/wake"
	writeProfileForTest(t, s)
	fake.reply("/usr/bin/uname -m", "arm64")
	probeTransport = probesAnswer{}

	text := Diagnose(context.Background(), DiagnoseOptions{SkipAWS: true})

	sections := []string{"microtunnel vpn diagnostics", "── this machine", "── VPN profiles", "── what is installed",
		"── configuration", "── tunnel", "── the service being published", "── networks this machine is on",
		"── supervisor", "── end"}
	last := -1
	for _, section := range sections {
		at := strings.Index(text, section)
		if at <= last {
			t.Errorf("%q is missing or out of order:\n%s", section, text)
		}
		last = at
	}
	for _, expected := range []string{"architecture", "arm64", "* default", "supervised             true",
		"exposed TCP ports      5432", "idle timeout           30 minutes", "wake role              arn:role/wake"} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "── aws") {
		t.Errorf("the AWS section ran although it was skipped:\n%s", text)
	}
}

func TestTheMachineSectionSaysWhatNeedsSudo(t *testing.T) {
	fake := stubMachine(t)
	text := reportOf(diagnoseMachine)
	if !strings.Contains(text, "running as             uid 501") || !strings.Contains(text, "need sudo") {
		t.Errorf("not said:\n%s", text)
	}
	if strings.Contains(text, "architecture") {
		t.Errorf("an architecture nobody reported was printed:\n%s", text)
	}

	fake.euid = 0
	if text := reportOf(diagnoseMachine); strings.Contains(text, "need sudo") {
		t.Errorf("root was told to use sudo:\n%s", text)
	}
}

func TestTheProfilesSectionMarksTheOneBeingDescribed(t *testing.T) {
	stubMachine(t)
	if text := reportOf(func(r *Report) { diagnoseProfiles(r, "work") }); !strings.Contains(text, "none installed") {
		t.Errorf("an empty store is not said to be empty:\n%s", text)
	}

	work := valid()
	work.ProfileName = "work"
	work.InterfaceName = "wg1"
	writeProfileForTest(t, work)
	home := valid()
	home.ProfileName = "home"
	writeProfileForTest(t, home)

	text := reportOf(func(r *Report) { diagnoseProfiles(r, "work") })
	if !strings.Contains(text, "* work") || !strings.Contains(text, "  home") || strings.Contains(text, "* home") {
		t.Errorf("the current profile is not the marked one:\n%s", text)
	}
	if !strings.Contains(text, "wg1  10.100.0.2  mymicrotunnel-123456789012-us-east-2") {
		t.Errorf("the profile's interface and address are missing:\n%s", text)
	}
}

func TestTheInstallSectionReportsEachFileAndTheKeysPresence(t *testing.T) {
	fake := stubMachine(t)
	fake.place(t, HelperPath, "", 0o755)
	fake.place(t, "/etc/wireguard/wg0.key", "secret", 0o644)
	fake.reply(HelperPath+" version", "1.4.2")
	if err := os.MkdirAll(ProfileDir(DefaultProfileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PublicKeyPath(DefaultProfileName), []byte("public-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	text := reportOf(func(r *Report) { diagnoseInstall(r, diagnosedConfig(), Settings{}) })

	for _, expected := range []string{
		shortName(HelperPath) + strings.Repeat(" ", 1),
		"helper version         1.4.2",
		shortName(CommandPath) + strings.Repeat(" ", 22-len(shortName(CommandPath))) + " MISSING",
		"private key            present, mode -rw-r--r--",
		"WARNING: expected mode 600",
		"public key             public-key",
		"sudoers rule           MISSING",
		"service port           3000",
		"health check           https://updates.example.com/hc",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "secret") {
		t.Errorf("the private key's contents were printed:\n%s", text)
	}
	if strings.Contains(text, "idle timeout") {
		t.Errorf("a deployment with no idle timeout reported one:\n%s", text)
	}
}

func TestAMissingPrivateKeyIsReportedWhereItWasLookedFor(t *testing.T) {
	stubMachine(t)
	text := reportOf(func(r *Report) { diagnoseInstall(r, diagnosedConfig(), Settings{}) })
	if !strings.Contains(text, "private key            MISSING at /etc/wireguard/wg0.key") {
		t.Errorf("not reported:\n%s", text)
	}
}

// "Cannot look" is a third answer, distinct from "not there".
func TestAKeyThatCannotBeLookedAtIsNotCalledMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can look at anything")
	}
	stubMachine(t)
	guarded := onDisk("/etc/wireguard")
	if err := os.MkdirAll(guarded, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(guarded, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(guarded, 0o755) })

	text := reportOf(func(r *Report) { diagnoseInstall(r, diagnosedConfig(), Settings{}) })
	if !strings.Contains(text, "cannot check without sudo (/etc/wireguard is root-only)") {
		t.Errorf("not reported:\n%s", text)
	}
}

func TestTheSudoersRuleIsJudgedByVisudo(t *testing.T) {
	fake := stubMachine(t)
	if state := sudoersState(); !strings.HasPrefix(state, "MISSING") {
		t.Errorf("a missing rule is %q", state)
	}

	fake.place(t, SudoersPath, "rule", 0o440)
	fake.reply("/usr/sbin/visudo", "parsed OK")
	if state := sudoersState(); state != "present, visudo accepts it" {
		t.Errorf("an accepted rule is %q", state)
	}

	// visudo refusing to open the file is not a syntax error, and saying it is
	// sends the reader looking for one.
	fake.refuse("/usr/sbin/visudo", "visudo: unable to open "+SudoersPath)
	if state := sudoersState(); state != "present; run with sudo to validate it" {
		t.Errorf("an unreadable rule is %q", state)
	}

	fake.euid = 0
	fake.refuse("/usr/sbin/visudo", "syntax error near line 3\nmore detail")
	if state := sudoersState(); state != "present but visudo rejects it: syntax error near line 3" {
		t.Errorf("a rejected rule is %q", state)
	}
}

func TestASudoersRuleThatCannotBeLookedAtSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can look at anything")
	}
	stubMachine(t)
	guarded := onDisk(filepath.Dir(SudoersPath))
	if err := os.MkdirAll(guarded, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(guarded, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(guarded, 0o755) })

	if state := sudoersState(); state != "cannot check without sudo" {
		t.Errorf("an unreadable rule is %q", state)
	}
}

// --- the tunnel --------------------------------------------------------------

func TestTheTunnelSectionTellsUpFromCarryingTraffic(t *testing.T) {
	fake := stubMachine(t)
	config := diagnosedConfig()

	if text := reportOf(func(r *Report) { diagnoseTunnel(r, config) }); !strings.Contains(text, "state                  down") {
		t.Errorf("a missing tunnel is not down:\n%s", text)
	}

	fake.present["10.100.0.2"] = true
	if text := reportOf(func(r *Report) { diagnoseTunnel(r, config) }); !strings.Contains(text, "up (10.100.0.2 is assigned)") {
		t.Errorf("an assigned address is not up:\n%s", text)
	}

	fake.devices["wg0"] = "utun4"
	fake.status["wg0"] = tunnel.Status{Peers: []tunnel.PeerStatus{
		{PublicKey: "gateway-key", Endpoint: "203.0.113.10:51820", LastHandshake: time.Now().Add(-time.Minute),
			ReceivedBytes: 10, SentBytes: 20},
		{PublicKey: "never-key"},
	}}
	text := reportOf(func(r *Report) { diagnoseTunnel(r, config) })
	for _, expected := range []string{"up on utun4", "peer                   gateway-key", "203.0.113.10:51820",
		"ago", "rx 10  tx 20", "handshake            never", "endpoint             (none)", "carrying traffic       true"} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}

	fake.status["wg0"] = tunnel.Status{}
	if text := reportOf(func(r *Report) { diagnoseTunnel(r, config) }); !strings.Contains(text, "none configured") {
		t.Errorf("a tunnel with no peers is not explained:\n%s", text)
	}
}

// Linux shows the interface but not its state without root.
func TestATunnelThatCannotBeReadWithoutRootIsNotCalledBroken(t *testing.T) {
	fake := stubMachine(t)
	fake.devices["wg0"] = "wg0"
	fake.present["10.100.0.2"] = true
	fake.statusErr = errors.New("operation not permitted")

	text := reportOf(func(r *Report) { diagnoseTunnel(r, diagnosedConfig()) })
	if !strings.Contains(text, "run with sudo for detail") || strings.Contains(text, "could not read") {
		t.Errorf("not explained:\n%s", text)
	}

	fake.euid = 0
	text = reportOf(func(r *Report) { diagnoseTunnel(r, diagnosedConfig()) })
	if !strings.Contains(text, "handshake              could not read: operation not permitted") {
		t.Errorf("root's failure to read is not reported:\n%s", text)
	}
}

// --- the service ------------------------------------------------------------

func TestTheServiceSectionNamesTheLoopbackMistake(t *testing.T) {
	fake := stubMachine(t)
	fake.present["10.100.0.2"] = true
	probeTransport = probesAnswer{"127.0.0.1:3000": http.StatusNotFound, "updates.example.com": http.StatusOK}

	text := reportOf(func(r *Report) { diagnoseService(r, diagnosedConfig()) })

	for _, expected := range []string{"on loopback            404 Not Found", "on the tunnel          unreachable (",
		"it is bound to loopback", "public hostname        200 OK"} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}
}

func TestTheServiceSectionOnlyExplainsWhatIsTrue(t *testing.T) {
	fake := stubMachine(t)
	probeTransport = probesAnswer{}

	text := reportOf(func(r *Report) { diagnoseService(r, diagnosedConfig()) })
	if !strings.Contains(text, "the tunnel is down, so the second probe proves nothing") ||
		!strings.Contains(text, "public hostname        unreachable") {
		t.Errorf("not explained:\n%s", text)
	}

	fake.present["10.100.0.2"] = true
	text = reportOf(func(r *Report) { diagnoseService(r, diagnosedConfig()) })
	if !strings.Contains(text, "the service is not answering on port 3000 at all") {
		t.Errorf("not explained:\n%s", text)
	}

	probeTransport = probesAnswer{"127.0.0.1:3000": http.StatusOK, "10.100.0.2:3000": http.StatusOK}
	config := diagnosedConfig()
	config.HealthCheckURL = ""
	text = reportOf(func(r *Report) { diagnoseService(r, config) })
	if strings.Contains(text, "PROBLEM") || strings.Contains(text, "note") || strings.Contains(text, "public hostname") {
		t.Errorf("a working service was given a finding:\n%s", text)
	}
}

func TestAServiceWithNoPortIsSkipped(t *testing.T) {
	stubMachine(t)
	config := diagnosedConfig()
	config.ServicePort = ""
	if text := reportOf(func(r *Report) { diagnoseService(r, config) }); !strings.Contains(text, "no port recorded") {
		t.Errorf("not skipped:\n%s", text)
	}
}

// --- AWS ---------------------------------------------------------------------

func TestTheAWSSectionMasksTheAccount(t *testing.T) {
	stubMachine(t)
	cloud := stubAWS(t)
	writeProfileForTest(t, valid())
	cloud.reply("sts GetCallerIdentity", queryOK("GetCallerIdentity",
		`<Arn>arn:aws:iam::123456789012:user/alice</Arn><Account>123456789012</Account><UserId>u</UserId>`))
	cloud.reply("cloudformation DescribeStacks", stackWithOutputs("CREATE_COMPLETE", map[string]string{
		"GatewayPublicIp": "203.0.113.10", "ServiceUrl": "https://updates.example.com", "TargetGroupArn": "arn:tg/main"}))
	cloud.reply("ssm GetParameter", jsonOK(`{"Parameter":{"Name":"peers","Value":"[{\"publicKey\":\"k\",\"address\":\"10.100.0.2\",\"label\":\"laptop\"}]"}}`))
	cloud.reply("elasticloadbalancing DescribeTargetHealth", targetHealth("10.100.0.2", "healthy"))

	text := reportOf(func(r *Report) {
		diagnoseAWS(context.Background(), r, DiagnoseOptions{ProfileName: DefaultProfileName, StackName: "stack"})
	})

	for _, expected := range []string{"region                 eu-west-1", "arn:aws:iam::…9012:user/alice",
		"gateway address        203.0.113.10", "registered             10.100.0.2", "laptop",
		"target health          healthy"} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "123456789012") {
		t.Errorf("the whole account number was printed:\n%s", text)
	}
}

func TestTheAWSSectionSaysHowFarItGot(t *testing.T) {
	for name, scenario := range map[string]struct {
		options DiagnoseOptions
		change  func(*fakeAWS)
		want    string
	}{
		"no credentials": {DiagnoseOptions{Profile: "nobody", Region: "eu-west-1"}, func(*fakeAWS) {},
			"credentials            could not load"},
		"refused credentials": {DiagnoseOptions{}, func(cloud *fakeAWS) {
			cloud.reply("sts GetCallerIdentity", queryError("InvalidClientTokenId", "bad"))
		}, "credentials            do not work"},
		"no stack": {DiagnoseOptions{StackName: "stack"}, func(cloud *fakeAWS) {
			cloud.reply("cloudformation DescribeStacks", stackMissing())
		}, "stack                  could not read"},
		"no peers": {DiagnoseOptions{StackName: "stack"}, func(cloud *fakeAWS) {
			cloud.reply("ssm GetParameter", jsonError("ParameterNotFound", "none"))
		}, "empty — no workstation is registered"},
		"unreadable peers": {DiagnoseOptions{StackName: "stack"}, func(cloud *fakeAWS) {
			cloud.reply("ssm GetParameter", jsonError("AccessDeniedException", "no"))
		}, "peer registry          could not read"},
		"unreadable health": {DiagnoseOptions{StackName: "stack"}, func(cloud *fakeAWS) {
			cloud.reply("elasticloadbalancing DescribeTargetHealth", queryError("AccessDenied", "no"))
		}, "target health          could not read"},
	} {
		stubMachine(t)
		cloud := stubAWS(t)
		cloud.reply("sts GetCallerIdentity", queryOK("GetCallerIdentity", `<Arn>arn:aws:iam::123:user/a</Arn>`))
		cloud.reply("cloudformation DescribeStacks", stackWithOutputs("CREATE_COMPLETE", map[string]string{"TargetGroupArn": "arn:tg/main"}))
		cloud.reply("ssm GetParameter", jsonOK(`{"Parameter":{"Name":"peers","Value":"[]"}}`))
		cloud.reply("elasticloadbalancing DescribeTargetHealth", targetHealth("10.100.0.2", "healthy"))
		scenario.change(cloud)

		text := reportOf(func(r *Report) { diagnoseAWS(context.Background(), r, scenario.options) })
		if !strings.Contains(text, scenario.want) {
			t.Errorf("%s: %q is missing:\n%s", name, scenario.want, text)
		}
	}
}

// --- local networks ----------------------------------------------------------

func TestTheNetworksSectionListsCollisionsSideBySide(t *testing.T) {
	fake := stubMachine(t)
	installedProfile(t, fake, DefaultProfileName, "wg0", true)
	fake.networks = []tunnel.Network{network(t, "en0", "172.31.4.0/24")}

	text := reportOf(func(r *Report) { diagnoseLocalNetworks(r, diagnosedConfig()) })

	for _, expected := range []string{"en0                    172.31.4.0/24", "tunnel would route     10.100.0.2/32, 10.100.0.0/24, 172.31.0.0/16",
		"THIS TUNNEL WILL NOT BE RAISED", "en0 is already on 172.31.4.0/24"} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}

	fake.networks = []tunnel.Network{network(t, "en0", "192.168.1.0/24")}
	if text := reportOf(func(r *Report) { diagnoseLocalNetworks(r, diagnosedConfig()) }); !strings.Contains(text, "collisions             none") {
		t.Errorf("no collision is not said:\n%s", text)
	}
}

func TestTheNetworksSectionSurvivesWhatItCannotRead(t *testing.T) {
	fake := stubMachine(t)
	if text := reportOf(func(r *Report) { diagnoseLocalNetworks(r, diagnosedConfig()) }); !strings.Contains(text, "the configuration could not be read") {
		t.Errorf("a missing configuration is not reported:\n%s", text)
	}

	fake.networksErr = errors.New("netlink refused")
	if text := reportOf(func(r *Report) { diagnoseLocalNetworks(r, diagnosedConfig()) }); !strings.Contains(text, "could not be listed: netlink refused") {
		t.Errorf("unlistable interfaces are not reported:\n%s", text)
	}
}

// --- the supervisor ------------------------------------------------------------

func TestTheSupervisorSectionNarratesItsLastDecisions(t *testing.T) {
	fake := stubMachine(t)
	if text := reportOf(func(r *Report) { diagnoseSupervisor(r, DefaultProfileName) }); !strings.Contains(text, "installed              no") {
		t.Errorf("an absent supervisor is not said to be absent:\n%s", text)
	}

	fake.place(t, SupervisorPath, "job", 0o644)
	unloaded := answerSupervisorUnloaded(fake)
	text := reportOf(func(r *Report) { diagnoseSupervisor(r, DefaultProfileName) })
	if !strings.Contains(text, unloaded) || !strings.Contains(text, "desired state          not recorded") {
		t.Errorf("an unloaded supervisor is not reported:\n%s", text)
	}

	answerSupervisorState(fake, "running", "bringing wg0 up\nre-pin failed")
	if err := SetDesiredState(DefaultProfileName, true); err != nil {
		t.Fatal(err)
	}
	text = reportOf(func(r *Report) { diagnoseSupervisor(r, DefaultProfileName) })
	for _, expected := range []string{"installed              " + SupervisorPath, "running",
		"desired state          up", "last log lines:", "    bringing wg0 up", "    re-pin failed"} {
		if !strings.Contains(text, expected) {
			t.Errorf("%q is missing:\n%s", expected, text)
		}
	}
}

// --- small helpers -----------------------------------------------------------

func TestTheAccountIsReducedToItsLastFourDigits(t *testing.T) {
	for arn, want := range map[string]string{
		"arn:aws:iam::123456789012:user/alice": "arn:aws:iam::…9012:user/alice",
		"arn:aws:iam::12:user/alice":           "arn:aws:iam::12:user/alice",
		"not-an-arn":                           "not-an-arn",
	} {
		if got := maskAccount(arn); got != want {
			t.Errorf("maskAccount(%q) = %q, want %q", arn, got, want)
		}
	}
}

func TestOwnerNamesTheFilesUserAndGroup(t *testing.T) {
	if got := owner(filepath.Join(t.TempDir(), "missing")); got != "unknown" {
		t.Errorf("a missing file is owned by %q", got)
	}
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := owner(path); !strings.Contains(got, ":") || got == "unknown" {
		t.Errorf("owner is %q", got)
	}
}

func TestTheReportHelpers(t *testing.T) {
	if shortName("/usr/local/lib/mymicrotunnel/") != "mymicrotunnel" {
		t.Errorf("shortName kept the trailing slash: %q", shortName("/usr/local/lib/mymicrotunnel/"))
	}
	if firstLine("one\ntwo") != "one" || firstLine("only") != "only" {
		t.Error("firstLine")
	}
	if orNone("  ") != "(none)" || orNone("x") != "x" {
		t.Error("orNone")
	}
	if max(1, 2) != 2 || max(3, 2) != 3 {
		t.Error("max")
	}
	long := strings.Repeat("x", 70)
	if text := reportOf(func(r *Report) { r.section(long) }); !strings.HasSuffix(text, long+" \n") {
		t.Errorf("a long title was padded: %q", text)
	}
}
