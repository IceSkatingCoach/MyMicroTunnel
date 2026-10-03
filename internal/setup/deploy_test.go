// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
)

func awsClient(t *testing.T) *awsops.Client {
	t.Helper()
	client, err := awsops.LoadProfile(context.Background(), "", "eu-west-1")
	if err != nil {
		t.Fatalf("loading the fake account: %v", err)
	}
	return client
}

// --- the client key ----------------------------------------------------------

// A recorded public key is what lets a re-run recognise its own tunnel without
// asking for root first.
func TestARecordedPublicKeyIsReused(t *testing.T) {
	fake := stubMachine(t)
	s := valid()
	if err := os.MkdirAll(ProfileDir(s.ProfileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PublicKeyPath(s.ProfileName), []byte("recorded-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var public string
	captured(t, func() { public = EnsureClientKey(&s, true) })

	if public != "recorded-key" {
		t.Errorf("public key is %q, want the recorded one", public)
	}
	if len(fake.commands) != 0 {
		t.Errorf("asked root for a key it already knew: %v", fake.commands)
	}
}

// Overwriting a private key leaves the gateway trusting a public half nobody
// holds the other half of.
func TestAnExistingPrivateKeyIsKeptAndItsPublicHalfRecorded(t *testing.T) {
	fake := stubMachine(t)
	s := valid()
	private, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := tunnel.PublicKey(private)
	fake.reply("/usr/bin/sudo -n cat "+s.ClientKeyPath(), private+"\n")

	var public string
	captured(t, func() { public = EnsureClientKey(&s, false) })

	if public != want {
		t.Errorf("public key is %q, want %q", public, want)
	}
	if recorded, _ := os.ReadFile(PublicKeyPath(s.ProfileName)); strings.TrimSpace(string(recorded)) != want {
		t.Errorf("the public key was not recorded: %q", recorded)
	}
	if s.StagedKeyPath != "" {
		t.Errorf("a key was staged although one exists: %s", s.StagedKeyPath)
	}
}

func TestAFileThatIsNotAKeyStopsTheInstall(t *testing.T) {
	fake := stubMachine(t)
	s := valid()
	fake.reply("/usr/bin/sudo -n cat", "not a key")

	var message string
	captured(t, func() { message = failureOf(func() { EnsureClientKey(&s, false) }) })

	if !strings.Contains(message, s.ClientKeyPath()+" does not hold a WireGuard key") {
		t.Errorf("failed with %q", message)
	}
}

// A non-interactive run cannot answer a password prompt, so it never asks.
func TestANewKeyIsStagedForThePrivilegedStage(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		fake := stubMachine(t)
		s := valid()
		s.ProfileName = "work"

		var public string
		captured(t, func() { public = EnsureClientKey(&s, interactive) })

		if fake.ran("/usr/bin/sudo cat") != interactive {
			t.Errorf("interactive=%t: prompted for a password: %v", interactive, fake.commands)
		}
		if s.StagedKeyPath != filepath.Join(os.TempDir(), "mymicrotunnel-staging", "work", "client.key") {
			t.Errorf("staged at %s", s.StagedKeyPath)
		}
		info, err := os.Stat(s.StagedKeyPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("the staged key is not private: %v %v", info, err)
		}
		private, _ := os.ReadFile(s.StagedKeyPath)
		if derived, err := tunnel.PublicKey(string(private)); err != nil || derived != public {
			t.Errorf("the returned public key %q is not the staged key's (%q, %v)", public, derived, err)
		}
	}
}

func TestAStagingDirectoryThatCannotBeCreatedStopsTheInstall(t *testing.T) {
	stubMachine(t)
	blocker := filepath.Join(os.TempDir(), "mymicrotunnel-staging")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := valid()

	var message string
	captured(t, func() { message = failureOf(func() { EnsureClientKey(&s, false) }) })

	if !strings.HasPrefix(message, "Could not create "+stagingDir(s.ProfileName)) {
		t.Errorf("failed with %q", message)
	}
}

func TestAnEmptyPublicKeyIsNeverRecorded(t *testing.T) {
	stubMachine(t)
	if message := failureOf(func() { recordPublicKey("work", "") }); message != "`wg pubkey` produced nothing." {
		t.Errorf("failed with %q", message)
	}
	if _, err := os.Stat(PublicKeyPath("work")); !os.IsNotExist(err) {
		t.Errorf("an empty key was written: %v", err)
	}
}

func TestAnUnnamedProfileStagesUnderTheDefault(t *testing.T) {
	if stagingDir("") != stagingDir(DefaultProfileName) {
		t.Errorf("%s and %s differ", stagingDir(""), stagingDir(DefaultProfileName))
	}
}

// --- discovery -----------------------------------------------------------------

func answerDiscovery(cloud *fakeAWS) {
	cloud.reply("ec2 DescribeVpcs", ec2OK("DescribeVpcs",
		`<vpcSet><item><vpcId>vpc-1</vpcId><cidrBlock>172.31.0.0/16</cidrBlock><isDefault>true</isDefault></item></vpcSet>`))
	cloud.reply("ec2 DescribeRouteTables", ec2OK("DescribeRouteTables",
		`<routeTableSet><item><routeTableId>rtb-1</routeTableId>`+
			`<associationSet><item><main>true</main></item></associationSet>`+
			`<routeSet><item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><gatewayId>igw-1</gatewayId></item></routeSet>`+
			`</item></routeTableSet>`))
	cloud.reply("ec2 DescribeSubnets", ec2OK("DescribeSubnets",
		`<subnetSet><item><subnetId>subnet-a</subnetId><availabilityZone>eu-west-1a</availabilityZone></item>`+
			`<item><subnetId>subnet-b</subnetId><availabilityZone>eu-west-1b</availabilityZone></item></subnetSet>`))
	cloud.reply("route53 GET /2013-04-01/hostedzone", awsReply{Body: `<ListHostedZonesResponse>` +
		`<HostedZones><HostedZone><Id>/hostedzone/ZEXAMPLE</Id><Name>example.com.</Name><CallerReference>x</CallerReference></HostedZone></HostedZones>` +
		`<IsTruncated>false</IsTruncated><MaxItems>100</MaxItems></ListHostedZonesResponse>`})
}

func TestDiscoveryFillsInTheAccount(t *testing.T) {
	stubMachine(t)
	cloud := stubAWS(t)
	answerDiscovery(cloud)
	s := valid()
	s.VpcID, s.VpcCidr, s.HostedZoneID = "", "", ""
	s.SubnetIDs, s.GatewaySubnetIDs, s.RouteTableIDs = nil, nil, nil

	output := captured(t, func() { Discover(context.Background(), awsClient(t), &s) })

	if s.VpcID != "vpc-1" || s.VpcCidr != "172.31.0.0/16" || s.HostedZoneID != "ZEXAMPLE" {
		t.Errorf("discovered %s %s %s", s.VpcID, s.VpcCidr, s.HostedZoneID)
	}
	if strings.Join(s.SubnetIDs, ",") != "subnet-a,subnet-b" || strings.Join(s.RouteTableIDs, ",") != "rtb-1" {
		t.Errorf("subnets %v, route tables %v", s.SubnetIDs, s.RouteTableIDs)
	}
	if !strings.Contains(output, "VPC vpc-1 (172.31.0.0/16), 2 public subnet(s)") ||
		!strings.Contains(output, "Hosted zone ZEXAMPLE is authoritative for updates.example.com") {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestDiscoveryStopsAtWhatTheAccountCannotProvide(t *testing.T) {
	for name, scenario := range map[string]struct {
		change func(*fakeAWS, *Settings)
		want   string
	}{
		"no VPC": {func(cloud *fakeAWS, _ *Settings) {
			cloud.reply("ec2 DescribeVpcs", ec2OK("DescribeVpcs", `<vpcSet/>`))
		}, "no VPC in eu-west-1"},
		"no zone": {func(_ *fakeAWS, s *Settings) {
			s.DomainName = "updates.example.org"
		}, "no public Route53 hosted zone"},
		"tunnel inside the VPC": {func(_ *fakeAWS, s *Settings) {
			s.ClientAddress = "172.31.9.9"
		}, "falls inside the VPC range"},
	} {
		stubMachine(t)
		cloud := stubAWS(t)
		answerDiscovery(cloud)
		s := valid()
		s.VpcID, s.HostedZoneID = "", ""
		scenario.change(cloud, &s)

		var message string
		captured(t, func() {
			message = failureOf(func() { Discover(context.Background(), awsClient(t), &s) })
		})
		if !strings.Contains(message, scenario.want) {
			t.Errorf("%s: failed with %q, want %q", name, message, scenario.want)
		}
	}
}

// --- deploying ---------------------------------------------------------------

var deployedOutputs = map[string]string{
	"GatewayPublicIp":          "203.0.113.10",
	"TargetGroupArn":           "arn:tg/main",
	"WakeRoleArn":              "arn:role/wake",
	"GatewayGroupName":         "gateway-group",
	"LoadBalancerDnsName":      "lb.example.aws",
	"LoadBalancerHostedZoneId": "ZLB",
	"TcpTarget1":               "5432:5432=arn:tg/5432",
}

func answerDeploy(cloud *fakeAWS, outputs map[string]string) {
	cloud.sequence("cloudformation DescribeStacks", stackMissing(), stackWithOutputs("CREATE_COMPLETE", outputs))
	cloud.reply("cloudformation GetTemplateSummary", queryOK("GetTemplateSummary",
		`<Parameters><member><ParameterKey>ServiceDomainName</ParameterKey></member>`+
			`<member><ParameterKey>VpcId</ParameterKey></member></Parameters>`))
	cloud.reply("cloudformation CreateChangeSet", queryOK("CreateChangeSet", `<Id>arn:changeset</Id>`))
	cloud.reply("cloudformation DescribeChangeSet", queryOK("DescribeChangeSet", `<Status>CREATE_COMPLETE</Status>`))
	cloud.reply("cloudformation ExecuteChangeSet", queryOK("ExecuteChangeSet", ""))
	cloud.reply("cloudformation DescribeStackEvents", queryOK("DescribeStackEvents",
		`<StackEvents><member><LogicalResourceId>Gateway</LogicalResourceId><ResourceStatus>CREATE_COMPLETE</ResourceStatus></member></StackEvents>`))
	cloud.reply("route53 POST /2013-04-01/hostedzone/*", awsReply{Body: `<ChangeResourceRecordSetsResponse><ChangeInfo>` +
		`<Id>c</Id><Status>PENDING</Status><SubmittedAt>2020-01-01T00:00:00Z</SubmittedAt></ChangeInfo></ChangeResourceRecordSetsResponse>`})
	cloud.reply("ssm GetParameter", jsonOK(`{"Parameter":{"Name":"key","Value":"server-key"}}`))
}

func TestADeployRecordsWhatTheStackReportedBack(t *testing.T) {
	stubMachine(t)
	cloud := stubAWS(t)
	answerDeploy(cloud, deployedOutputs)
	s := valid()

	output := captured(t, func() { Deploy(context.Background(), awsClient(t), &s) })

	if s.Endpoint != "203.0.113.10" || s.TargetGroupARN != "arn:tg/main" || s.WakeRoleARN != "arn:role/wake" ||
		s.GatewayGroupName != "gateway-group" || s.ServerPublicKey != "server-key" {
		t.Errorf("recorded %+v", s)
	}
	if s.TcpTargetGroups["5432:5432"] != "arn:tg/5432" {
		t.Errorf("TCP target groups %v", s.TcpTargetGroups)
	}
	if s.LoadBalancerDNSName != "lb.example.aws" || s.LoadBalancerZoneID != "ZLB" {
		t.Errorf("alias target %s %s", s.LoadBalancerDNSName, s.LoadBalancerZoneID)
	}

	created := cloud.made("cloudformation CreateChangeSet")
	if len(created) != 1 || created[0].Form.Get("ChangeSetType") != "CREATE" ||
		!strings.Contains(created[0].Body, "updates.example.com") {
		t.Errorf("change set %+v", created)
	}
	alias := cloud.made("route53 POST /2013-04-01/hostedzone/*")
	if len(alias) != 1 || !strings.Contains(alias[0].Body, "<Action>UPSERT</Action>") ||
		!strings.Contains(alias[0].Body, "updates.example.com.") {
		t.Errorf("alias %+v", alias)
	}
	for _, expected := range []string{"Gateway", "1 extra TCP port(s) exposed", "updates.example.com is an alias for lb.example.aws", "Gateway key retrieved"} {
		if !strings.Contains(output, expected) {
			t.Errorf("%q not reported:\n%s", expected, output)
		}
	}
}

func TestADeployStopsAtTheFirstThingThatFails(t *testing.T) {
	for name, scenario := range map[string]struct {
		change func(*fakeAWS, context.CancelFunc)
		want   string
	}{
		"change set refused": {func(cloud *fakeAWS, _ context.CancelFunc) {
			cloud.reply("cloudformation CreateChangeSet", queryError("ValidationError", "bad template"))
		}, "The deployment failed"},
		"outputs unreadable": {func(cloud *fakeAWS, _ context.CancelFunc) {
			cloud.sequence("cloudformation DescribeStacks", stackMissing(),
				stackWithOutputs("CREATE_COMPLETE", deployedOutputs), queryError("Throttled", "slow down"))
		}, "Could not read the stack outputs"},
		"no gateway": {func(cloud *fakeAWS, _ context.CancelFunc) {
			cloud.sequence("cloudformation DescribeStacks", stackMissing(), stackWithOutputs("CREATE_COMPLETE", nil))
		}, "did not report a gateway address and a target group"},
		"alias refused": {func(cloud *fakeAWS, _ context.CancelFunc) {
			cloud.reply("route53 POST /2013-04-01/hostedzone/*", awsReply{Status: http.StatusBadRequest,
				Body: `<ErrorResponse><Error><Code>InvalidInput</Code><Message>no</Message></Error></ErrorResponse>`})
		}, "Could not write the DNS record for updates.example.com"},
		"no server key": {func(cloud *fakeAWS, cancel context.CancelFunc) {
			cloud.on("ssm GetParameter", func(awsCall) awsReply {
				cancel()
				return jsonError("ParameterNotFound", "not yet")
			})
		}, "cloud-init-output.log"},
	} {
		stubMachine(t)
		cloud := stubAWS(t)
		answerDeploy(cloud, deployedOutputs)
		ctx, cancel := context.WithCancel(context.Background())
		scenario.change(cloud, cancel)
		s := valid()
		client := awsClient(t)

		var message string
		captured(t, func() { message = failureOf(func() { Deploy(ctx, client, &s) }) })
		cancel()
		if !strings.Contains(message, scenario.want) {
			t.Errorf("%s: failed with %q, want %q", name, message, scenario.want)
		}
	}
}

// --- registering this machine ------------------------------------------------

func registrable() Settings {
	s := valid()
	s.PeerLabel = "alices-laptop"
	s.TargetGroupARN = "arn:tg/main"
	s.TcpPorts = []string{"5432", "3000:8080"}
	s.TcpTargetGroups = map[string]string{"5432:5432": "arn:tg/5432"}
	return s
}

func TestRegisteringPutsThisMachineInBothLists(t *testing.T) {
	stubMachine(t)
	cloud := stubAWS(t)
	cloud.reply("ssm GetParameter", jsonOK(`{"Parameter":{"Name":"peers","Value":"[{\"publicKey\":\"other\",\"address\":\"10.100.0.3\"}]"}}`))
	cloud.reply("ssm PutParameter", jsonOK(`{"Version":2}`))
	cloud.reply("elasticloadbalancing RegisterTargets", queryOK("RegisterTargets", ""))

	output := captured(t, func() { RegisterWorkstation(context.Background(), awsClient(t), registrable(), "my-public-key") })

	put := cloud.made("ssm PutParameter")
	if len(put) != 1 || !strings.Contains(put[0].Body, `my-public-key`) || !strings.Contains(put[0].Body, `alices-laptop`) ||
		!strings.Contains(put[0].Body, `other`) {
		t.Errorf("peer list written as %+v", put)
	}
	registered := cloud.made("elasticloadbalancing RegisterTargets")
	if len(registered) != 2 {
		t.Fatalf("%d registrations, want the service and port 5432", len(registered))
	}
	if registered[0].Form.Get("Targets.member.1.Port") != "3000" || registered[1].Form.Get("TargetGroupArn") != "arn:tg/5432" ||
		registered[1].Form.Get("Targets.member.1.Port") != "5432" {
		t.Errorf("registered %v and %v", registered[0].Form, registered[1].Form)
	}
	if !strings.Contains(output, "2 workstation(s) in the peer list") ||
		!strings.Contains(output, "The stack exposes no target group for 3000:8080") {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestRegisteringStopsWhenAListRefusesThisMachine(t *testing.T) {
	for name, scenario := range map[string]struct {
		change func(*fakeAWS)
		want   string
	}{
		"peer list": {func(cloud *fakeAWS) {
			cloud.reply("ssm PutParameter", jsonError("AccessDeniedException", "no"))
		}, "Could not register this machine as a peer"},
		"service target": {func(cloud *fakeAWS) {
			cloud.reply("elasticloadbalancing RegisterTargets", queryError("AccessDenied", "no"))
		}, "Could not register 10.100.0.2 behind the load balancer"},
		"TCP target": {func(cloud *fakeAWS) {
			cloud.on("elasticloadbalancing RegisterTargets", func(call awsCall) awsReply {
				if call.Form.Get("TargetGroupArn") == "arn:tg/5432" {
					return queryError("AccessDenied", "no")
				}
				return queryOK("RegisterTargets", "")
			})
		}, "Could not register 10.100.0.2:5432 behind the load balancer"},
	} {
		stubMachine(t)
		cloud := stubAWS(t)
		cloud.reply("ssm GetParameter", jsonError("ParameterNotFound", "none"))
		cloud.reply("ssm PutParameter", jsonOK(`{"Version":1}`))
		cloud.reply("elasticloadbalancing RegisterTargets", queryOK("RegisterTargets", ""))
		scenario.change(cloud)

		var message string
		captured(t, func() {
			message = failureOf(func() { RegisterWorkstation(context.Background(), awsClient(t), registrable(), "key") })
		})
		if !strings.HasPrefix(message, scenario.want) {
			t.Errorf("%s: failed with %q, want %q", name, message, scenario.want)
		}
	}
}

// --- verifying the path ------------------------------------------------------

func verifiable(t *testing.T, fake *fakeMachine, cloud *fakeAWS) Settings {
	t.Helper()
	s := valid()
	s.TargetGroupARN = "arn:tg/main"
	fake.reply("/usr/bin/sudo -n "+HelperPath+" tunnel up wg0", "")
	fake.reply("ping", "")
	cloud.reply("elasticloadbalancing DescribeTargetHealth", targetHealth("10.100.0.2", "healthy"))
	probeTransport = probesAnswer{"updates.example.com": http.StatusOK}
	return s
}

func TestVerifyWalksThePathInTheOrderTrafficTakes(t *testing.T) {
	fake := stubMachine(t)
	cloud := stubAWS(t)
	s := verifiable(t, fake, cloud)

	output := captured(t, func() { Verify(context.Background(), awsClient(t), s) })

	steps := []string{"Tunnel is up", "Gateway 10.100.0.1 answers", "Load balancer target is healthy",
		"https://updates.example.com/hc returns 200"}
	last := -1
	for _, step := range steps {
		at := strings.Index(output, step)
		if at < last {
			t.Errorf("%q is missing or out of order:\n%s", step, output)
		}
		last = at
	}
	if !fake.ran("ping -c 2") {
		t.Errorf("the gateway was not pinged: %v", fake.commands)
	}
}

// A source checkout has no installed helper yet; an install started with sudo
// can raise the tunnel itself.
func TestVerifyRaisesTheTunnelItselfWithoutTheHelper(t *testing.T) {
	fake := stubMachine(t)
	cloud := stubAWS(t)
	s := verifiable(t, fake, cloud)
	fake.refuse("/usr/bin/sudo -n "+HelperPath+" tunnel up wg0", "sudo: a password is required")
	installedProfile(t, fake, DefaultProfileName, "wg0", true)

	captured(t, func() { Verify(context.Background(), awsClient(t), s) })

	if len(fake.raised) != 1 || fake.raised[0].Name != "wg0" {
		t.Errorf("raised %+v", fake.raised)
	}

	fake.raised = nil
	if err := os.Remove(onDisk(s.TunnelConfigPath())); err != nil {
		t.Fatal(err)
	}
	var message string
	captured(t, func() { message = failureOf(func() { Verify(context.Background(), awsClient(t), s) }) })
	if !strings.HasPrefix(message, "The tunnel would not come up") || !strings.Contains(message, "a password is required") {
		t.Errorf("failed with %q", message)
	}
}

// A gateway with an idle timeout may have switched itself off before anyone
// used it; failing to wake it is reported, and the checks go on.
func TestVerifyWakesAGatewayThatMayHaveSwitchedItselfOff(t *testing.T) {
	fake := stubMachine(t)
	cloud := stubAWS(t)
	s := verifiable(t, fake, cloud)
	s.IdleTimeoutMinutes = 30
	s.GatewayGroupName = "gateway-group"
	cloud.reply("autoscaling DescribeAutoScalingGroups", queryError("AccessDenied", "no"))

	output := captured(t, func() { Verify(context.Background(), awsClient(t), s) })

	if len(cloud.made("autoscaling DescribeAutoScalingGroups")) != 1 {
		t.Error("the gateway was not asked about")
	}
	if !strings.Contains(output, "Could not confirm the gateway is running") || !strings.Contains(output, "returns 200") {
		t.Errorf("not reported, or the checks stopped:\n%s", output)
	}
}

func TestVerifyStopsAtTheFirstHopThatDoesNotAnswer(t *testing.T) {
	for name, scenario := range map[string]struct {
		change func(*fakeMachine, *fakeAWS)
		want   string
	}{
		"gateway silent": {func(fake *fakeMachine, _ *fakeAWS) {
			fake.refuse("ping", "100.0% packet loss")
		}, "The gateway at 10.100.0.1 did not answer"},
		"target unreadable": {func(_ *fakeMachine, cloud *fakeAWS) {
			cloud.reply("elasticloadbalancing DescribeTargetHealth", queryError("AccessDenied", "no"))
		}, "AccessDenied"},
		"target unhealthy": {func(_ *fakeMachine, cloud *fakeAWS) {
			cloud.reply("elasticloadbalancing DescribeTargetHealth", targetHealth("10.100.0.2", "unhealthy"))
		}, `still reports this machine as "unhealthy"`},
		"hostname silent": {func(*fakeMachine, *fakeAWS) {
			probeTransport = probesAnswer{}
		}, "https://updates.example.com/hc did not answer"},
		"hostname failing": {func(*fakeMachine, *fakeAWS) {
			probeTransport = probesAnswer{"updates.example.com": http.StatusServiceUnavailable}
		}, "https://updates.example.com/hc returned 503."},
	} {
		fake := stubMachine(t)
		cloud := stubAWS(t)
		s := verifiable(t, fake, cloud)
		scenario.change(fake, cloud)

		var message string
		captured(t, func() { message = failureOf(func() { Verify(context.Background(), awsClient(t), s) }) })
		if !strings.Contains(message, scenario.want) {
			t.Errorf("%s: failed with %q, want %q", name, message, scenario.want)
		}
	}
}
