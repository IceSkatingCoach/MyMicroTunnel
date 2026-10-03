// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A trust policy that names an IAM Identity Center *session* is a role nobody
// can assume tomorrow: the session name changes on every sign-in, so the wake
// would work on the day of the install and never again.
func TestTrustablePrincipalReducesASessionToItsRole(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Admin_abc/user@example.com": "arn:aws:iam::123456789012:role/AWSReservedSSO_Admin_abc",
		// Already stable: used as it stands.
		"arn:aws:iam::123456789012:user/deployer": "arn:aws:iam::123456789012:user/deployer",
		"arn:aws:iam::123456789012:role/Deployer": "arn:aws:iam::123456789012:role/Deployer",
		// Another partition keeps its own: "aws" is not hardcoded.
		"arn:aws-us-gov:sts::123456789012:assumed-role/Deployer/session": "arn:aws-us-gov:iam::123456789012:role/Deployer",
		// Nothing recognisable is returned untouched rather than mangled into
		// an ARN that would be rejected at deploy time with no explanation.
		"not-an-arn":                        "not-an-arn",
		"":                                  "",
		"sts:assumed-role/Deployer/session": "sts:assumed-role/Deployer/session",
		"arn:aws:sts::123456789012:assumed-role/x": "arn:aws:sts::123456789012:assumed-role/x",
	}

	for caller, want := range cases {
		if got := TrustablePrincipal(caller); got != want {
			t.Errorf("TrustablePrincipal(%q) = %q, want %q", caller, got, want)
		}
	}
}

// groupXML renders the one Auto Scaling group, with an instance per
// lifecycle state given.
func groupXML(desired int, states ...string) awsReply {
	var instances strings.Builder
	for _, state := range states {
		instances.WriteString("<member><InstanceId>i-1</InstanceId><LifecycleState>" + state + "</LifecycleState></member>")
	}
	return queryOK("DescribeAutoScalingGroups", "<AutoScalingGroups><member><AutoScalingGroupName>asg</AutoScalingGroupName>"+
		"<DesiredCapacity>"+strconv.Itoa(desired)+"</DesiredCapacity><Instances>"+instances.String()+"</Instances></member></AutoScalingGroups>")
}

func recordProgress() (*[]string, func(string)) {
	var lines []string
	return &lines, func(line string) { lines = append(lines, line) }
}

func TestWakeGatewayNeedsAGroup(t *testing.T) {
	fake := newFakeAWS(t)
	err := fake.client().WakeGateway(context.Background(), WakeOptions{})
	if err == nil || !strings.Contains(err.Error(), "no Auto Scaling group") {
		t.Errorf("got %v", err)
	}
	if calls := fake.operations(); len(calls) != 0 {
		t.Errorf("called AWS anyway: %v", calls)
	}
}

func TestWakeGatewayLeavesARunningGatewayAlone(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("autoscaling DescribeAutoScalingGroups", groupXML(1, "Terminating", "InService"))
	progress, onProgress := recordProgress()

	if err := fake.client().WakeGateway(context.Background(), WakeOptions{GroupName: "asg", Wait: time.Minute, OnProgress: onProgress}); err != nil {
		t.Fatal(err)
	}
	if len(fake.made("autoscaling SetDesiredCapacity")) != 0 {
		t.Error("asked a running group for another instance")
	}
	if !slices.Equal(*progress, []string{"the gateway is already running"}) {
		t.Errorf("progress %v", *progress)
	}
	if name := fake.made("autoscaling DescribeAutoScalingGroups")[0].Form.Get("AutoScalingGroupNames.member.1"); name != "asg" {
		t.Errorf("described %q", name)
	}
}

func TestWakeGatewayAsksForExactlyOneInstance(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("autoscaling DescribeAutoScalingGroups", groupXML(0))
	fake.reply("autoscaling SetDesiredCapacity", queryOK("SetDesiredCapacity", ""))

	// No OnProgress: a nudge from the menu bar does not listen for any.
	if err := fake.client().WakeGateway(context.Background(), WakeOptions{GroupName: "asg"}); err != nil {
		t.Fatal(err)
	}
	set := fake.made("autoscaling SetDesiredCapacity")
	if len(set) != 1 {
		t.Fatalf("operations: %v", fake.operations())
	}
	form := set[0].Form
	if form.Get("AutoScalingGroupName") != "asg" || form.Get("DesiredCapacity") != "1" || form.Get("HonorCooldown") != "false" {
		t.Errorf("unexpected request: %v", form)
	}
	if polls := len(fake.made("autoscaling DescribeAutoScalingGroups")); polls != 1 {
		t.Errorf("waited (%d reads) when not asked to", polls)
	}
}

func TestWakeGatewayWaitsUntilTheInstanceIsInService(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	// Already scaled up by somebody else: the group is not asked again.
	fake.sequence("autoscaling DescribeAutoScalingGroups",
		groupXML(1, "Pending"),
		groupXML(1, "Pending"),
		groupXML(1, "InService"))
	progress, onProgress := recordProgress()

	if err := fake.client().WakeGateway(context.Background(), WakeOptions{GroupName: "asg", Wait: time.Minute, OnProgress: onProgress}); err != nil {
		t.Fatal(err)
	}
	if len(fake.made("autoscaling SetDesiredCapacity")) != 0 {
		t.Error("a group already asking for one instance was asked again")
	}
	want := []string{"asked for a gateway instance", "waiting for the gateway to boot", "the gateway is in service"}
	if !slices.Equal(*progress, want) {
		t.Errorf("progress %v, want %v", *progress, want)
	}
}

func TestWakeGatewayGivesUpAfterTheWait(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.reply("autoscaling DescribeAutoScalingGroups", groupXML(1, "Pending"))

	err := fake.client().WakeGateway(context.Background(), WakeOptions{GroupName: "asg", Wait: 20 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "the gateway did not come back within 20ms") {
		t.Errorf("got %v", err)
	}
}

func TestWakeGatewayStopsWhenTheContextIsCancelled(t *testing.T) {
	slowPolling(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeAWS(t)
	var once sync.Once
	fake.on("autoscaling DescribeAutoScalingGroups", func(awsCall) awsReply {
		once.Do(func() { time.AfterFunc(50*time.Millisecond, cancel) })
		return groupXML(1)
	})

	if err := fake.client().WakeGateway(ctx, WakeOptions{GroupName: "asg", Wait: time.Hour}); err != context.Canceled {
		t.Errorf("got %v, want context.Canceled", err)
	}
}

func TestWakeGatewaySurfacesGroupFailures(t *testing.T) {
	fastPolling(t)
	cases := map[string]struct {
		describe []awsReply
		set      awsReply
		want     string
	}{
		"unreadable group": {
			describe: []awsReply{queryError(http.StatusForbidden, "AccessDenied", "denied")},
			want:     "reading the Auto Scaling group",
		},
		"missing group": {
			describe: []awsReply{queryOK("DescribeAutoScalingGroups", "<AutoScalingGroups></AutoScalingGroups>")},
			want:     "no Auto Scaling group called asg",
		},
		"refused scale-up": {
			describe: []awsReply{groupXML(0)},
			set:      queryError(http.StatusBadRequest, "ScalingActivityInProgress", "busy"),
			want:     "asking for a gateway instance",
		},
		"group gone while waiting": {
			describe: []awsReply{groupXML(1), queryError(http.StatusForbidden, "AccessDenied", "denied")},
			want:     "reading the Auto Scaling group",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeAWS(t)
			fake.sequence("autoscaling DescribeAutoScalingGroups", test.describe...)
			fake.reply("autoscaling SetDesiredCapacity", test.set)
			err := fake.client().WakeGateway(context.Background(), WakeOptions{GroupName: "asg", Wait: time.Minute})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want %q", err, test.want)
			}
		})
	}
}

const assumedRole = `<Credentials><AccessKeyId>ASIAWAKE</AccessKeyId><SecretAccessKey>wake-secret</SecretAccessKey>` +
	`<SessionToken>wake-token</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>` +
	`<AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/wake/mymicrotunnel-wake</Arn><AssumedRoleId>AROA:wake</AssumedRoleId></AssumedRoleUser>`

// The wake role exists so a laptop's routine wake does not carry a credential
// that could delete the deployment; every scaling call has to go out under it.
func TestWakeGatewayScalesUnderTheWakeRole(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("sts AssumeRole", queryOK("AssumeRole", assumedRole))
	fake.reply("autoscaling DescribeAutoScalingGroups", groupXML(0))
	fake.reply("autoscaling SetDesiredCapacity", queryOK("SetDesiredCapacity", ""))

	err := fake.client().WakeGateway(context.Background(), WakeOptions{RoleARN: "arn:aws:iam::123456789012:role/wake", GroupName: "asg"})
	if err != nil {
		t.Fatal(err)
	}

	assumed := fake.made("sts AssumeRole")
	if len(assumed) != 1 {
		t.Fatalf("operations: %v", fake.operations())
	}
	if form := assumed[0].Form; form.Get("RoleArn") != "arn:aws:iam::123456789012:role/wake" || form.Get("RoleSessionName") != "mymicrotunnel-wake" {
		t.Errorf("unexpected AssumeRole: %v", form)
	}
	if !strings.Contains(assumed[0].Header.Get("Authorization"), "Credential=AKIDTEST/") {
		t.Error("the role was not assumed with the caller's own credentials")
	}
	for _, operation := range []string{"autoscaling DescribeAutoScalingGroups", "autoscaling SetDesiredCapacity"} {
		call := fake.made(operation)[0]
		if !strings.Contains(call.Header.Get("Authorization"), "Credential=ASIAWAKE/") || call.Header.Get("X-Amz-Security-Token") != "wake-token" {
			t.Errorf("%s was not signed with the wake role", operation)
		}
	}
}

func TestWakeGatewayExplainsARoleItCannotAssume(t *testing.T) {
	fake := newFakeAWS(t)
	client := fake.client()
	options := WakeOptions{RoleARN: "arn:aws:iam::123456789012:role/wake", GroupName: "asg"}

	fake.reply("sts AssumeRole", queryError(http.StatusForbidden, "AccessDenied", "not authorized to perform sts:AssumeRole"))
	if err := client.WakeGateway(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "this AWS profile may not assume arn:aws:iam::123456789012:role/wake") {
		t.Errorf("got %v", err)
	}

	fake.reply("sts AssumeRole", queryError(http.StatusBadRequest, "RegionDisabledException", "STS is not activated"))
	if err := client.WakeGateway(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "assuming arn:aws:iam::123456789012:role/wake") {
		t.Errorf("got %v", err)
	}
	if len(fake.made("autoscaling DescribeAutoScalingGroups")) != 0 {
		t.Error("touched the group without the role")
	}
}

func callerIdentity() awsReply {
	return queryOK("GetCallerIdentity", "<Arn>arn:aws:iam::123456789012:user/deployer</Arn><UserId>AIDA</UserId><Account>123456789012</Account>")
}

func TestIdentityAndAccountIDComeFromSTS(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("sts GetCallerIdentity", callerIdentity())
	client := fake.client()

	if arn, err := client.Identity(context.Background()); err != nil || arn != "arn:aws:iam::123456789012:user/deployer" {
		t.Errorf("Identity: got %q, %v", arn, err)
	}
	if account, err := client.AccountID(context.Background()); err != nil || account != "123456789012" {
		t.Errorf("AccountID: got %q, %v", account, err)
	}

	fake.reply("sts GetCallerIdentity", queryError(http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid."))
	if _, err := client.Identity(context.Background()); err == nil || !strings.Contains(err.Error(), "InvalidClientTokenId") {
		t.Errorf("Identity: got %v", err)
	}
	if _, err := client.AccountID(context.Background()); err == nil || !strings.Contains(err.Error(), "InvalidClientTokenId") {
		t.Errorf("AccountID: got %v", err)
	}
}
