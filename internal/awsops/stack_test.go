// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func templateSummary(keys ...string) awsReply {
	var members strings.Builder
	for _, key := range keys {
		members.WriteString("<member><ParameterKey>" + key + "</ParameterKey></member>")
	}
	return queryOK("GetTemplateSummary", "<Parameters>"+members.String()+"</Parameters>")
}

func changeSetStatus(status, reason string) awsReply {
	return queryOK("DescribeChangeSet", "<Status>"+status+"</Status><StatusReason>"+reason+"</StatusReason>")
}

// stackEvents takes events newest first, the way the API lists them, as
// "LogicalId STATUS" or "LogicalId STATUS reason".
func stackEvents(events ...string) awsReply {
	var members strings.Builder
	for _, event := range events {
		fields := strings.SplitN(event, " ", 3)
		members.WriteString("<member><StackName>stack</StackName><LogicalResourceId>" + fields[0] +
			"</LogicalResourceId><ResourceStatus>" + fields[1] + "</ResourceStatus>")
		if len(fields) == 3 {
			members.WriteString("<ResourceStatusReason>" + fields[2] + "</ResourceStatusReason>")
		}
		members.WriteString("</member>")
	}
	return queryOK("DescribeStackEvents", "<StackEvents>"+members.String()+"</StackEvents>")
}

// deployableFake answers a first deployment that succeeds; each test then
// replaces whichever answer it is about.
func deployableFake(t *testing.T) *fakeAWS {
	t.Helper()
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.sequence("cloudformation DescribeStacks",
		stackMissing(),
		describeStacks("CREATE_IN_PROGRESS", nil, nil),
		describeStacks("CREATE_COMPLETE", nil, nil))
	fake.reply("cloudformation GetTemplateSummary", templateSummary("Hostname"))
	fake.reply("cloudformation CreateChangeSet", queryOK("CreateChangeSet", "<Id>cs</Id>"))
	fake.reply("cloudformation DescribeChangeSet", changeSetStatus("CREATE_COMPLETE", ""))
	fake.reply("cloudformation ExecuteChangeSet", queryOK("ExecuteChangeSet", ""))
	fake.reply("cloudformation DescribeStackEvents", stackEvents())
	fake.reply("cloudformation DeleteChangeSet", queryOK("DeleteChangeSet", ""))
	return fake
}

func ignoreProgress(string) {}

func TestDeployStackCreatesAStackThatDoesNotExistYet(t *testing.T) {
	fake := deployableFake(t)

	err := fake.client().DeployStack(context.Background(), "stack", "Resources: {}",
		map[string]string{"Hostname": "vpn.example.com"}, ignoreProgress)
	if err != nil {
		t.Fatal(err)
	}

	created := fake.made("cloudformation CreateChangeSet")
	if len(created) != 1 {
		t.Fatalf("got %d change sets, want 1", len(created))
	}
	form := created[0].Form
	if form.Get("ChangeSetType") != "CREATE" || form.Get("StackName") != "stack" ||
		form.Get("TemplateBody") != "Resources: {}" || form.Get("Capabilities.member.1") != "CAPABILITY_IAM" {
		t.Errorf("unexpected change set request: %v", form)
	}
	if form.Get("Parameters.member.1.ParameterKey") != "Hostname" ||
		form.Get("Parameters.member.1.ParameterValue") != "vpn.example.com" {
		t.Errorf("the hostname was not sent: %v", form)
	}
	executed := fake.made("cloudformation ExecuteChangeSet")
	if len(executed) != 1 || executed[0].Form.Get("ChangeSetName") != form.Get("ChangeSetName") {
		t.Errorf("the change set that was created is not the one executed: %v", executed)
	}
}

// A stack left in REVIEW_IN_PROGRESS has a change set that was never
// executed; an UPDATE against it is rejected.
func TestDeployStackTreatsAnUnexecutedStackAsNew(t *testing.T) {
	fake := deployableFake(t)
	fake.sequence("cloudformation DescribeStacks",
		describeStacks("REVIEW_IN_PROGRESS", nil, nil),
		describeStacks("CREATE_COMPLETE", nil, nil))

	if err := fake.client().DeployStack(context.Background(), "stack", "t", nil, ignoreProgress); err != nil {
		t.Fatal(err)
	}
	if got := fake.made("cloudformation CreateChangeSet")[0].Form.Get("ChangeSetType"); got != "CREATE" {
		t.Errorf("change set type %q, want CREATE", got)
	}
}

func TestDeployStackUpdateCarriesOverOnlyDeclaredParameters(t *testing.T) {
	fake := deployableFake(t)
	fake.sequence("cloudformation DescribeStacks",
		describeStacks("UPDATE_COMPLETE", map[string]string{"Hostname": "old", "AmiId": "ami-1", "Dropped": "x"}, nil),
		describeStacks("UPDATE_COMPLETE", map[string]string{"Hostname": "old", "AmiId": "ami-1", "Dropped": "x"}, nil),
		describeStacks("UPDATE_IN_PROGRESS", nil, nil),
		describeStacks("UPDATE_COMPLETE", nil, nil))
	fake.reply("cloudformation GetTemplateSummary", templateSummary("Hostname", "AmiId", "Port"))
	fake.sequence("cloudformation DescribeChangeSet",
		changeSetStatus("CREATE_PENDING", ""),
		changeSetStatus("CREATE_COMPLETE", ""))
	fake.sequence("cloudformation DescribeStackEvents",
		stackEvents("Gateway CREATE_COMPLETE"),
		stackEvents("stack UPDATE_COMPLETE", "Alarm UPDATE_COMPLETE", "Pending UPDATE_IN_PROGRESS", "Gateway CREATE_COMPLETE"))

	var progress []string
	err := fake.client().DeployStack(context.Background(), "stack", "t",
		map[string]string{"Hostname": "new", "Port": "51820", "Undeclared": "y"},
		func(line string) { progress = append(progress, line) })
	if err != nil {
		t.Fatal(err)
	}

	form := fake.made("cloudformation CreateChangeSet")[0].Form
	if form.Get("ChangeSetType") != "UPDATE" {
		t.Errorf("change set type %q, want UPDATE", form.Get("ChangeSetType"))
	}
	// Sorted by key; AmiId is carried rather than re-resolved, and neither a
	// parameter the template dropped nor one it never had is sent.
	want := map[string]string{
		"Parameters.member.1.ParameterKey":     "AmiId",
		"Parameters.member.1.UsePreviousValue": "true",
		"Parameters.member.2.ParameterKey":     "Hostname",
		"Parameters.member.2.ParameterValue":   "new",
		"Parameters.member.3.ParameterKey":     "Port",
		"Parameters.member.3.ParameterValue":   "51820",
	}
	for key, value := range want {
		if form.Get(key) != value {
			t.Errorf("%s = %q, want %q", key, form.Get(key), value)
		}
	}
	if form.Has("Parameters.member.1.ParameterValue") || form.Has("Parameters.member.4.ParameterKey") {
		t.Errorf("unexpected parameters sent: %v", form)
	}
	if len(fake.made("cloudformation DescribeChangeSet")) != 2 {
		t.Error("the pending change set was not polled again")
	}
	// Oldest first, each resource once, and never the stack itself.
	if !slices.Equal(progress, []string{"Gateway", "Alarm"}) {
		t.Errorf("progress %v, want [Gateway Alarm]", progress)
	}
}

func TestDeployStackWithNothingToChangeIsANoOp(t *testing.T) {
	for _, reason := range []string{
		"The submitted information didn't contain changes. Submit different information to create a change set.",
		"No updates are to be performed.",
	} {
		fake := deployableFake(t)
		fake.sequence("cloudformation DescribeStacks", describeStacks("UPDATE_COMPLETE", nil, nil))
		fake.reply("cloudformation DescribeChangeSet", changeSetStatus("FAILED", reason))

		var progress []string
		err := fake.client().DeployStack(context.Background(), "stack", "t", nil,
			func(line string) { progress = append(progress, line) })
		if err != nil {
			t.Fatalf("%q: %v", reason, err)
		}
		if len(fake.made("cloudformation ExecuteChangeSet")) != 0 {
			t.Errorf("%q: an empty change set was executed", reason)
		}
		deleted := fake.made("cloudformation DeleteChangeSet")
		created := fake.made("cloudformation CreateChangeSet")
		if len(deleted) != 1 || deleted[0].Form.Get("ChangeSetName") != created[0].Form.Get("ChangeSetName") {
			t.Errorf("%q: the empty change set was not cleaned up: %v", reason, deleted)
		}
		if !slices.Equal(progress, []string{"No changes to apply"}) {
			t.Errorf("%q: progress %v", reason, progress)
		}
	}
}

func TestDeployStackSurfacesEachFailure(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		reply     awsReply
		want      string
	}{
		{"describing the stack", "cloudformation DescribeStacks", queryError(http.StatusForbidden, "AccessDenied", "not allowed"), "not allowed"},
		{"reading the template", "cloudformation GetTemplateSummary", queryError(http.StatusBadRequest, "ValidationError", "bad yaml"), "reading the template's parameters"},
		{"creating the change set", "cloudformation CreateChangeSet", queryError(http.StatusBadRequest, "ValidationError", "nope"), "creating the change set"},
		{"describing the change set", "cloudformation DescribeChangeSet", queryError(http.StatusBadRequest, "ChangeSetNotFound", "gone"), "gone"},
		{"a failed change set", "cloudformation DescribeChangeSet", changeSetStatus("FAILED", "Template error: bad ref"), "the change set failed: Template error: bad ref"},
		{"executing the change set", "cloudformation ExecuteChangeSet", queryError(http.StatusBadRequest, "InvalidChangeSetStatus", "stale"), "executing the change set"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fake := deployableFake(t)
			fake.reply(test.operation, test.reply)
			err := fake.client().DeployStack(context.Background(), "stack", "t", nil, ignoreProgress)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("got %v, want an error mentioning %q", err, test.want)
			}
		})
	}
}

func TestDeployStackUpdateFailsWhenTheParametersCannotBeRead(t *testing.T) {
	fake := deployableFake(t)
	fake.sequence("cloudformation DescribeStacks",
		describeStacks("UPDATE_COMPLETE", nil, nil),
		queryError(http.StatusBadRequest, "Throttling", "slow down"))

	err := fake.client().DeployStack(context.Background(), "stack", "t", nil, ignoreProgress)
	if err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("got %v, want the throttling error", err)
	}
	if len(fake.made("cloudformation CreateChangeSet")) != 0 {
		t.Error("a change set was created without knowing which parameters to carry")
	}
}

func TestDeployStackReportsWhyTheStackRolledBack(t *testing.T) {
	fake := deployableFake(t)
	fake.sequence("cloudformation DescribeStacks", stackMissing(), describeStacks("ROLLBACK_COMPLETE", nil, nil))
	fake.reply("cloudformation DescribeStackEvents", stackEvents(
		"stack ROLLBACK_COMPLETE",
		"A CREATE_FAILED a", "B CREATE_FAILED b", "C CREATE_FAILED c",
		"D CREATE_FAILED d", "E CREATE_FAILED e", "F CREATE_FAILED f",
		"Fine CREATE_COMPLETE"))

	err := fake.client().DeployStack(context.Background(), "stack", "t", nil, ignoreProgress)
	if err == nil {
		t.Fatal("a rolled-back stack was reported as deployed")
	}
	message := err.Error()
	if !strings.Contains(message, "ROLLBACK_COMPLETE") || !strings.Contains(message, "  A: a") || !strings.Contains(message, "  E: e") {
		t.Errorf("the reasons are missing: %s", message)
	}
	// Five reasons are enough to act on; a cascade of every dependent
	// resource failing after the first is noise.
	if strings.Contains(message, "F: f") {
		t.Errorf("more than five reasons: %s", message)
	}
}

func TestWaitForStackStopsWhenTheStackDisappears(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStacks", queryOK("DescribeStacks", "<Stacks></Stacks>"))
	fake.reply("cloudformation DescribeStackEvents", stackEvents())

	err := fake.client().waitForStack(context.Background(), "stack", ignoreProgress)
	if err == nil || !strings.Contains(err.Error(), "stack stack disappeared") {
		t.Errorf("got %v", err)
	}
}

func TestWaitForStackSurfacesADescribeError(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStacks", queryError(http.StatusForbidden, "AccessDenied", "denied"))

	if err := fake.client().waitForStack(context.Background(), "stack", ignoreProgress); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("got %v", err)
	}
}

// Progress is decoration: a stack whose events cannot be read still deploys.
func TestWaitForStackIgnoresUnreadableEvents(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.sequence("cloudformation DescribeStacks", describeStacks("UPDATE_IN_PROGRESS", nil, nil), describeStacks("UPDATE_COMPLETE", nil, nil))
	fake.reply("cloudformation DescribeStackEvents", queryError(http.StatusBadRequest, "Throttling", "slow down"))

	var progress []string
	err := fake.client().waitForStack(context.Background(), "stack", func(line string) { progress = append(progress, line) })
	if err != nil || len(progress) != 0 {
		t.Errorf("got %v with progress %v", err, progress)
	}
}

func TestWaitLoopsGiveUpEventually(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeChangeSet", changeSetStatus("CREATE_IN_PROGRESS", ""))
	fake.reply("cloudformation DescribeStacks", describeStacks("UPDATE_IN_PROGRESS", nil, nil))
	fake.reply("cloudformation DescribeStackEvents", stackEvents())
	fake.reply("cloudformation DeleteStack", queryOK("DeleteStack", ""))
	client := fake.client()

	if _, err := client.waitForChangeSet(context.Background(), "stack", "cs"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("change set: got %v", err)
	}
	if err := client.waitForStack(context.Background(), "stack", ignoreProgress); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("stack: got %v", err)
	}
	if err := client.DeleteStack(context.Background(), "stack"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("delete: got %v", err)
	}
}

// Every wait loop has to let go as soon as the caller does, rather than
// sleeping through the rest of its interval first.
func TestWaitLoopsStopWhenTheContextIsCancelled(t *testing.T) {
	pollers := map[string]func(*Client, context.Context) error{
		"change set": func(client *Client, ctx context.Context) error {
			_, err := client.waitForChangeSet(ctx, "stack", "cs")
			return err
		},
		"stack": func(client *Client, ctx context.Context) error {
			return client.waitForStack(ctx, "stack", ignoreProgress)
		},
		"delete": func(client *Client, ctx context.Context) error {
			return client.DeleteStack(ctx, "stack")
		},
		"parameter": func(client *Client, ctx context.Context) error {
			_, err := client.WaitForParameter(ctx, "/p", time.Hour)
			return err
		},
	}
	for name, poll := range pollers {
		t.Run(name, func(t *testing.T) {
			slowPolling(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := newFakeAWS(t)
			// Cancelled once the first answer is back, so the loop is asleep
			// when it happens rather than mid-request.
			var once sync.Once
			cancelling := func(reply awsReply) func(awsCall) awsReply {
				return func(awsCall) awsReply {
					once.Do(func() { time.AfterFunc(50*time.Millisecond, cancel) })
					return reply
				}
			}
			fake.on("cloudformation DescribeChangeSet", cancelling(changeSetStatus("CREATE_IN_PROGRESS", "")))
			fake.on("cloudformation DescribeStacks", cancelling(describeStacks("UPDATE_IN_PROGRESS", nil, nil)))
			fake.reply("cloudformation DescribeStackEvents", stackEvents())
			fake.reply("cloudformation DeleteStack", queryOK("DeleteStack", ""))
			fake.on("ssm GetParameter", cancelling(jsonError("ParameterNotFound", "")))

			started := time.Now()
			err := poll(fake.client(), ctx)
			if err != context.Canceled {
				t.Errorf("got %v, want context.Canceled", err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("took %s to notice the cancellation", elapsed)
			}
		})
	}
}

func TestDeleteStackWaitsUntilTheStackIsGone(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.reply("cloudformation DeleteStack", queryOK("DeleteStack", ""))
	fake.sequence("cloudformation DescribeStacks",
		describeStacks("DELETE_IN_PROGRESS", nil, nil),
		stackMissing())

	if err := fake.client().DeleteStack(context.Background(), "stack"); err != nil {
		t.Fatal(err)
	}
	if deleted := fake.made("cloudformation DeleteStack"); len(deleted) != 1 || deleted[0].Form.Get("StackName") != "stack" {
		t.Errorf("unexpected delete requests: %v", deleted)
	}
	if polls := len(fake.made("cloudformation DescribeStacks")); polls != 2 {
		t.Errorf("polled %d times, want 2", polls)
	}
}

func TestDeleteStackSurfacesARefusal(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DeleteStack", queryError(http.StatusForbidden, "AccessDenied", "no delete"))

	if err := fake.client().DeleteStack(context.Background(), "stack"); err == nil || !strings.Contains(err.Error(), "no delete") {
		t.Errorf("got %v", err)
	}
	if len(fake.made("cloudformation DescribeStacks")) != 0 {
		t.Error("waited for a deletion that was never started")
	}
}

func TestStackOutputsReadsEveryOutputInOneCall(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStacks", describeStacks("CREATE_COMPLETE", nil,
		map[string]string{"Endpoint": "nlb.example.com", "GroupName": "asg"}))

	outputs, err := fake.client().StackOutputs(context.Background(), "stack")
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 || outputs["Endpoint"] != "nlb.example.com" || outputs["GroupName"] != "asg" {
		t.Errorf("got %v", outputs)
	}
	if calls := fake.made("cloudformation DescribeStacks"); len(calls) != 1 || calls[0].Form.Get("StackName") != "stack" {
		t.Errorf("unexpected calls: %v", calls)
	}
}

func TestStackOutputsSurfacesAnError(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStacks", stackMissing())
	if _, err := fake.client().StackOutputs(context.Background(), "stack"); err == nil {
		t.Error("a missing stack has no outputs to report")
	}
}

func TestStackOutputFindsOneKey(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStacks", describeStacks("CREATE_COMPLETE", nil, map[string]string{"Endpoint": "nlb.example.com"}))
	client := fake.client()

	if value, err := client.StackOutput(context.Background(), "stack", "Endpoint"); err != nil || value != "nlb.example.com" {
		t.Errorf("got %q, %v", value, err)
	}
	if _, err := client.StackOutput(context.Background(), "stack", "Missing"); err == nil || !strings.Contains(err.Error(), "stack stack has no output Missing") {
		t.Errorf("got %v", err)
	}

	fake.reply("cloudformation DescribeStacks", stackMissing())
	if _, err := client.StackOutput(context.Background(), "stack", "Endpoint"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("got %v", err)
	}
}

func TestStackParameterReportsWhetherItIsSet(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStacks", describeStacks("CREATE_COMPLETE", map[string]string{"IdleMinutes": "30"}, nil))
	client := fake.client()

	if value, found := client.StackParameter(context.Background(), "stack", "IdleMinutes"); !found || value != "30" {
		t.Errorf("got %q, %v", value, found)
	}
	if value, found := client.StackParameter(context.Background(), "stack", "Other"); found || value != "" {
		t.Errorf("got %q, %v for a parameter the stack does not have", value, found)
	}

	fake.reply("cloudformation DescribeStacks", stackMissing())
	if _, found := client.StackParameter(context.Background(), "stack", "IdleMinutes"); found {
		t.Error("a missing stack reported a parameter")
	}
}

func TestFailureReasonsReportsAnUnreadableEventLog(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudformation DescribeStackEvents", queryError(http.StatusForbidden, "AccessDenied", "no events"))

	reasons := fake.client().FailureReasons(context.Background(), "stack")
	if len(reasons) != 1 || !strings.Contains(reasons[0], "no events") {
		t.Errorf("got %v", reasons)
	}
}
