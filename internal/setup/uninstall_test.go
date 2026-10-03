// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
)

// deployedHere is a profile as an install leaves it, with an AWS deployment
// that still lists this machine.
func deployedHere(t *testing.T, cloud *fakeAWS, name, interfaceName, address string) Settings {
	t.Helper()
	s := valid()
	s.ProfileName = name
	s.InterfaceName = interfaceName
	s.ClientAddress = address
	s.StackName = "stack-" + name
	writeProfileForTest(t, s)

	cloud.reply("ssm GetParameter", jsonOK(`{"Parameter":{"Name":"peers","Value":"[`+
		`{\"publicKey\":\"mine\",\"address\":\"`+address+`\"},{\"publicKey\":\"theirs\",\"address\":\"10.100.0.9\"}]"}}`))
	cloud.reply("ssm PutParameter", jsonOK(`{"Version":3}`))
	cloud.reply("cloudformation DescribeStacks", stackWithOutputs("CREATE_COMPLETE", map[string]string{
		"TargetGroupArn": "arn:tg/main",
		"TcpTarget1":     "5432:6543=arn:tg/6543",
		"TcpTarget2":     "nonsense=arn:tg/bad",
	}))
	cloud.reply("elasticloadbalancing DeregisterTargets", queryOK("DeregisterTargets", ""))
	return s
}

func uninstalling(t *testing.T) (*fakeMachine, *fakeAWS) {
	t.Helper()
	fake := stubMachine(t)
	fake.reply("/usr/sbin/visudo", "parsed OK")
	acceptSupervisor(fake)
	return fake, stubAWS(t)
}

func TestUninstallingTheLastProfileLeavesNothingBehind(t *testing.T) {
	fake, cloud := uninstalling(t)
	s := deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")
	s.AccountID = "123456789012"
	if err := SaveProfileSettings(s); err != nil {
		t.Fatal(err)
	}
	fake.place(t, SupervisorPath, "job", 0o644)
	var forgotten []string
	loadAppCredentials = func(account string) (*awsops.AppCredentials, error) {
		return &awsops.AppCredentials{AccessKeyID: "AKIAAPP"}, nil
	}
	forgetAppCredentials = func(account string) error {
		forgotten = append(forgotten, account)
		return nil
	}
	cloud.reply("iam DeleteAccessKey", queryOK("DeleteAccessKey", ""))

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{DeleteKeys: true}) })

	for _, command := range []string{
		"/usr/bin/sudo rm -f " + SupervisorPath,
		"/usr/bin/sudo " + HelperPath + " tunnel down wg0",
		"/usr/bin/sudo rm -f " + SudoersPath,
		"/usr/bin/sudo rm -f /etc/wireguard/wg0.key /etc/wireguard/wg0.conf",
	} {
		if !fake.ran(command) {
			t.Errorf("%q was not run: %v", command, fake.commands)
		}
	}
	if HasApp && !fake.ran("/usr/bin/pkill") {
		t.Errorf("the app was left running: %v", fake.commands)
	}

	put := cloud.made("ssm PutParameter")
	if len(put) != 1 || strings.Contains(put[0].Body, "mine") || !strings.Contains(put[0].Body, "theirs") {
		t.Errorf("the peer list was rewritten as %+v", put)
	}
	deregistered := cloud.made("elasticloadbalancing DeregisterTargets")
	if len(deregistered) != 2 {
		t.Fatalf("%d deregistrations, want the service and TCP 5432:6543", len(deregistered))
	}
	ports := map[string]string{}
	for _, call := range deregistered {
		ports[call.Form.Get("TargetGroupArn")] = call.Form.Get("Targets.member.1.Port")
	}
	// Deregistered with the local half: the registration was made with it, and
	// the published one would quietly leave the target in place.
	if ports["arn:tg/main"] != "3000" || ports["arn:tg/6543"] != "5432" {
		t.Errorf("deregistered %v", ports)
	}

	if len(cloud.made("iam DeleteAccessKey")) != 1 || strings.Join(forgotten, ",") != "123456789012" {
		t.Errorf("the application's key was left live or stored: forgotten %v", forgotten)
	}
	if _, err := os.Stat(AppConfigDir()); !os.IsNotExist(err) {
		t.Errorf("the app configuration was left: %v", err)
	}
	for _, expected := range []string{"Tunnel wg0 down", "Removed from the gateway's peer list",
		"Deregistered from the load balancer on TCP 5432:6543", "Profile default removed", "App configuration removed",
		"Application AWS credentials removed", "Tunnel key and configuration removed"} {
		if !strings.Contains(output, expected) {
			t.Errorf("%q not reported:\n%s", expected, output)
		}
	}
}

// Deleting one deployment and deleting this product are different requests.
func TestUninstallingOneProfileKeepsTheOthersWorking(t *testing.T) {
	fake, cloud := uninstalling(t)
	deployedHere(t, cloud, "work", "wg1", "10.110.0.2")
	kept := valid()
	kept.ProfileName = "home"
	kept.InterfaceName = "wg2"
	kept.Supervise = true
	writeProfileForTest(t, kept)

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{ProfileName: "work"}) })

	rule := fake.read(t, SudoersPath)
	if !strings.Contains(rule, "tunnel up wg2") || strings.Contains(rule, "wg1") {
		t.Errorf("the rule was not rewritten for what is left:\n%s", rule)
	}
	if fake.ran("/usr/bin/pkill") {
		t.Errorf("the app was removed although another profile uses it: %v", fake.commands)
	}
	if _, err := os.Stat(ProfileDir("work")); !os.IsNotExist(err) {
		t.Errorf("the profile was left: %v", err)
	}
	if _, err := os.Stat(ProfileDir("home")); err != nil {
		t.Errorf("the other profile was removed: %v", err)
	}
	if definition := fake.read(t, SupervisorPath); !strings.Contains(definition, ProfilesDir()) {
		t.Errorf("the supervisor was not reinstalled for the profile that wants it:\n%s", definition)
	}
	for _, expected := range []string{"Keeping the app: 1 other profile(s) still use it",
		"Sudoers rule rewritten for the remaining profiles", "Supervisor restarted for the remaining profiles"} {
		if !strings.Contains(output, expected) {
			t.Errorf("%q not reported:\n%s", expected, output)
		}
	}
}

func TestKeepingTheAppRemovesOnlyTheProfile(t *testing.T) {
	fake, cloud := uninstalling(t)
	deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")

	captured(t, func() { Uninstall(context.Background(), UninstallOptions{KeepApp: true}) })

	if fake.ran("/usr/bin/pkill") {
		t.Errorf("the app was removed: %v", fake.commands)
	}
	if _, err := os.Stat(AppConfigDir()); err != nil {
		t.Errorf("the app configuration was removed: %v", err)
	}
}

func TestAProblemWithTheRemainingProfilesIsReportedNotFatal(t *testing.T) {
	fake, cloud := uninstalling(t)
	deployedHere(t, cloud, "work", "wg1", "10.110.0.2")
	kept := valid()
	kept.ProfileName = "home"
	kept.Supervise = true
	writeProfileForTest(t, kept)
	fake.refuse("/usr/sbin/visudo", "syntax error")
	fake.writeErrors[SupervisorPath] = errors.New("read-only file system")

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{ProfileName: "work"}) })

	if !strings.Contains(output, "Leaving "+SudoersPath+" alone: the rewritten rule did not validate") ||
		!strings.Contains(output, "Could not restart the supervisor for the remaining profiles: read-only") {
		t.Errorf("not reported:\n%s", output)
	}
	if _, err := os.Stat(ProfileDir("work")); !os.IsNotExist(err) {
		t.Errorf("the uninstall stopped early: %v", err)
	}
}

func TestASudoersRuleThatCannotBeRewrittenIsReported(t *testing.T) {
	fake := stubMachine(t)
	fake.reply("/usr/sbin/visudo", "")
	fake.writeErrors[SudoersPath] = errors.New("sudo refused")

	output := captured(t, func() { rewriteSudoers([]Settings{valid()}) })

	if !strings.Contains(output, "Could not rewrite "+SudoersPath+": sudo refused") {
		t.Errorf("not reported:\n%s", output)
	}
}

// An uninstall must not stall because a credential expired months ago.
func TestWithdrawingFromAWSIsBestEffort(t *testing.T) {
	fake, cloud := uninstalling(t)
	deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")
	cloud.reply("ssm GetParameter", jsonError("AccessDeniedException", "no"))
	cloud.reply("elasticloadbalancing DeregisterTargets", queryError("AccessDenied", "no"))

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{KeepApp: true}) })

	for _, expected := range []string{"Could not remove this machine from the peer list",
		"Could not deregister 10.100.0.2 from the load balancer", "Could not deregister 10.100.0.2:5432"} {
		if !strings.Contains(output, expected) {
			t.Errorf("%q not reported:\n%s", expected, output)
		}
	}
	if !fake.ran("/usr/bin/sudo rm -f " + SudoersPath) {
		t.Errorf("the uninstall stopped at AWS: %v", fake.commands)
	}

	cloud.reply("cloudformation DescribeStacks", queryError("AccessDenied", "no"))
	output = captured(t, func() { Uninstall(context.Background(), UninstallOptions{KeepApp: true, StackName: "stack-default"}) })
	if !strings.Contains(output, "Could not read the stack outputs") {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestAnUnusableAWSProfileIsReportedAndTheRestIsRemoved(t *testing.T) {
	fake, cloud := uninstalling(t)
	deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{KeepApp: true, Profile: "nobody"}) })

	if !strings.Contains(output, "Not removing this machine from the AWS deployment") {
		t.Errorf("not reported:\n%s", output)
	}
	if !fake.ran("/usr/bin/sudo rm -f " + SudoersPath) {
		t.Errorf("the uninstall stopped at AWS: %v", fake.commands)
	}
}

func TestAKeyThatWillNotBeRevokedIsReported(t *testing.T) {
	_, cloud := uninstalling(t)
	s := deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")
	s.AccountID = "123456789012"
	if err := SaveProfileSettings(s); err != nil {
		t.Fatal(err)
	}
	loadAppCredentials = func(string) (*awsops.AppCredentials, error) {
		return &awsops.AppCredentials{AccessKeyID: "AKIAAPP"}, nil
	}
	forgetAppCredentials = func(string) error { return errors.New("keychain locked") }
	cloud.reply("iam DeleteAccessKey", queryError("AccessDenied", "no"))

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{}) })

	if !strings.Contains(output, "The application's AWS key is still live") ||
		!strings.Contains(output, "Could not remove the stored credentials: keychain locked") {
		t.Errorf("not reported:\n%s", output)
	}
}

// --- deleting the stack ------------------------------------------------------

func TestDeletingTheStackTakesItsRecordAndParametersWithIt(t *testing.T) {
	_, cloud := uninstalling(t)
	s := deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")
	s.LoadBalancerDNSName = "lb.example.aws"
	s.LoadBalancerZoneID = "ZLB"
	if err := SaveProfileSettings(s); err != nil {
		t.Fatal(err)
	}
	cloud.reply("route53 POST /2013-04-01/hostedzone/*", awsReply{Body: `<ChangeResourceRecordSetsResponse><ChangeInfo>` +
		`<Id>c</Id><Status>PENDING</Status><SubmittedAt>2020-01-01T00:00:00Z</SubmittedAt></ChangeInfo></ChangeResourceRecordSetsResponse>`})
	cloud.reply("cloudformation DeleteStack", queryOK("DeleteStack", ""))
	cloud.reply("cloudformation DescribeStacks", stackMissing())
	cloud.reply("ssm GetParametersByPath", jsonOK(`{"Parameters":[{"Name":"/stack-default/wireguard/peers"}]}`))
	cloud.reply("ssm DeleteParameters", jsonOK(`{"DeletedParameters":["/stack-default/wireguard/peers"]}`))

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{DeleteStack: true, KeepApp: true}) })

	// The stack takes both lists with it, so they are not edited first.
	if len(cloud.made("ssm PutParameter")) != 0 || len(cloud.made("elasticloadbalancing DeregisterTargets")) != 0 {
		t.Error("withdrew from a deployment that was about to be deleted")
	}
	alias := cloud.made("route53 POST /2013-04-01/hostedzone/*")
	if len(alias) != 1 || !strings.Contains(alias[0].Body, "<Action>DELETE</Action>") {
		t.Errorf("the alias was not deleted: %+v", alias)
	}
	deleted := cloud.made("cloudformation DeleteStack")
	if len(deleted) != 1 || deleted[0].Form.Get("StackName") != "stack-default" {
		t.Errorf("deleted %+v", deleted)
	}
	if !strings.Contains(cloud.made("ssm GetParametersByPath")[0].Body, "/stack-default/wireguard") {
		t.Error("the gateway's own parameters were not looked for under the stack's prefix")
	}
	for _, expected := range []string{"DNS record for updates.example.com removed", "Stack deleted",
		"Parameters under /stack-default/wireguard removed"} {
		if !strings.Contains(output, expected) {
			t.Errorf("%q not reported:\n%s", expected, output)
		}
	}
}

func TestDeletingTheStackReportsWhatItCouldNotTidy(t *testing.T) {
	_, cloud := uninstalling(t)
	s := deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")
	s.LoadBalancerDNSName = "lb.example.aws"
	s.LoadBalancerZoneID = "ZLB"
	if err := SaveProfileSettings(s); err != nil {
		t.Fatal(err)
	}
	cloud.reply("route53 POST /2013-04-01/hostedzone/*", awsReply{Status: 400,
		Body: `<ErrorResponse><Error><Code>AccessDenied</Code><Message>no</Message></Error></ErrorResponse>`})
	cloud.reply("cloudformation DeleteStack", queryOK("DeleteStack", ""))
	cloud.reply("cloudformation DescribeStacks", stackMissing())
	cloud.reply("ssm GetParametersByPath", jsonError("AccessDeniedException", "no"))

	output := captured(t, func() { Uninstall(context.Background(), UninstallOptions{DeleteStack: true, KeepApp: true}) })

	if !strings.Contains(output, "Could not remove the DNS record for updates.example.com") ||
		!strings.Contains(output, "Could not remove /stack-default/wireguard/*") {
		t.Errorf("not reported:\n%s", output)
	}
}

func TestAStackThatWillNotDeleteStopsTheUninstall(t *testing.T) {
	_, cloud := uninstalling(t)
	deployedHere(t, cloud, DefaultProfileName, "wg0", "10.100.0.2")
	cloud.reply("cloudformation DeleteStack", queryError("AccessDenied", "no"))

	var message string
	captured(t, func() {
		message = failureOf(func() { Uninstall(context.Background(), UninstallOptions{DeleteStack: true, KeepApp: true}) })
	})
	if !strings.HasPrefix(message, "Could not delete the stack") {
		t.Errorf("failed with %q", message)
	}

	captured(t, func() {
		message = failureOf(func() {
			Uninstall(context.Background(), UninstallOptions{DeleteStack: true, KeepApp: true, Profile: "nobody"})
		})
	})
	if !strings.HasPrefix(message, "Could not load AWS credentials") {
		t.Errorf("failed with %q", message)
	}
}

// An install that failed before writing its settings still wrote the config the
// menu bar reads, and that is what says which files are this profile's.
func TestThePathsToRemoveFallBackToTheAppConfig(t *testing.T) {
	config := appConfig{InterfaceName: "wg7"}
	if path := settingsKeyPath(Settings{}, config); path != "/etc/wireguard/wg7.key" {
		t.Errorf("key path %s", path)
	}
	if path := tunnelConfigPathOf(Settings{}, config); path != "/etc/wireguard/wg7.conf" {
		t.Errorf("config path %s", path)
	}
	recorded := Settings{InterfaceName: "wg3"}
	if settingsKeyPath(recorded, config) != "/etc/wireguard/wg3.key" || tunnelConfigPathOf(recorded, config) != "/etc/wireguard/wg3.conf" {
		t.Error("the recorded settings did not win")
	}
}
