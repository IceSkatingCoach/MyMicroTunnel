// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

// The commands, run in-process so coverage sees them. The e2e tests run the
// built binary and pin what the stages hand each other; these reach the
// branches a binary without AWS, root or a terminal never gets to.
//
// Nothing here runs in parallel: the seams, os.Stdout and HOME are all
// process-wide.

func stub[T any](t *testing.T, target *T, replacement T) {
	t.Helper()
	original := *target
	*target = replacement
	t.Cleanup(func() { *target = original })
}

// failure is what fail panics with in place of ending the test binary.
type failure struct{ message string }

type outcome struct {
	out     string
	failed  bool
	message string
}

// invoke runs one command the way main would, and returns what it printed and
// the message it stopped with, if it stopped.
func invoke(t *testing.T, command func([]string), args ...string) (result outcome) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("capturing stdout: %v", err)
	}
	original := os.Stdout
	os.Stdout = file
	defer func() {
		os.Stdout = original
		ui.SetJSON(false)
		recovered := recover()
		if stopped, ok := recovered.(failure); ok {
			result.failed = true
			result.message = stopped.message
		} else if recovered != nil {
			panic(recovered)
		}
		file.Close()
		content, _ := os.ReadFile(file.Name())
		result.out = string(content)
	}()
	command(args)
	return result
}

func (o outcome) mustSucceed(t *testing.T) outcome {
	t.Helper()
	if o.failed {
		t.Fatalf("the command failed: %s\noutput:\n%s", o.message, o.out)
	}
	return o
}

func (o outcome) mustFail(t *testing.T, want string) outcome {
	t.Helper()
	if !o.failed {
		t.Fatalf("the command succeeded; want a failure mentioning %q\noutput:\n%s", want, o.out)
	}
	if !strings.Contains(o.message, want) {
		t.Fatalf("the command failed with %q, want it to mention %q", o.message, want)
	}
	return o
}

func (o outcome) mustPrint(t *testing.T, wants ...string) outcome {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(o.out, want) {
			t.Errorf("the output does not mention %q:\n%s", want, o.out)
		}
	}
	return o
}

// isolate gives a test a home of its own and an AWS environment that cannot
// reach a real account, and replaces every step that would act on this
// machine with one that fails the test if it is reached unasked.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_ENDPOINT_URL", "SUDO_USER",
	} {
		t.Setenv(name, "")
	}
	// The SDK works out ~/.aws once, when the package loads, so a new HOME
	// alone would still read the real files of whoever runs the tests.
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(home, ".aws", "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, ".aws", "credentials"))
	// Without this a profile with no key falls through to the instance
	// metadata service, which is a real network.
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Cleanup(func() { ui.SetJSON(false) })

	stub(t, &fail, func(format string, args ...any) { panic(failure{fmt.Sprintf(format, args...)}) })
	stub(t, &ask, func(question, fallback string) string {
		t.Errorf("asked %q unexpectedly", question)
		return fallback
	})
	stub(t, &askSecret, func(question string) string {
		t.Errorf("asked %q unexpectedly", question)
		return ""
	})
	stub(t, &confirm, func(question string, fallback bool) bool {
		t.Errorf("asked %q unexpectedly", question)
		return fallback
	})
	// Not root, whoever runs the tests: the Linux container does.
	stub(t, &geteuid, func() int { return 1000 })

	stub(t, &loadAppCredentials, func(string) (*awsops.AppCredentials, error) { return nil, nil })
	stub(t, &ensureAppCredentials, func(context.Context, *awsops.Client, string) (*awsops.AppCredentials, error) {
		return nil, nil
	})

	unexpected := func(step string) { t.Errorf("%s ran unexpectedly", step) }
	stub(t, &prerequisites, func(bool) string { unexpected("prerequisites"); return "" })
	stub(t, &discover, func(context.Context, *awsops.Client, *setup.Settings) { unexpected("discover") })
	stub(t, &ensureClientKey, func(*setup.Settings, bool) string { unexpected("ensureClientKey"); return "" })
	stub(t, &deploy, func(context.Context, *awsops.Client, *setup.Settings) { unexpected("deploy") })
	stub(t, &registerWorkstation, func(context.Context, *awsops.Client, setup.Settings, string) {
		unexpected("registerWorkstation")
	})
	stub(t, &writeRootFiles, func(setup.Settings, string, bool) error { unexpected("writeRootFiles"); return nil })
	stub(t, &installApp, func(string) { unexpected("installApp") })
	stub(t, &registerLoginItem, func() { unexpected("registerLoginItem") })
	stub(t, &verify, func(context.Context, *awsops.Client, setup.Settings) { unexpected("verify") })
	stub(t, &openApp, func() { unexpected("openApp") })
	stub(t, &uninstall, func(context.Context, setup.UninstallOptions) { unexpected("uninstall") })
	stub(t, &superviseProfiles, func(setup.SuperviseOptions) { unexpected("supervise") })
	stub(t, &diagnose, func(context.Context, setup.DiagnoseOptions) string { unexpected("diagnose"); return "" })
	stub(t, &raiseTunnel, func(string, string) error { unexpected("raiseTunnel"); return nil })
	stub(t, &tunnelDown, func(string) error { unexpected("tunnelDown"); return nil })

	// Every interface is down unless a test says otherwise, so the answers do
	// not depend on what the machine running the tests has up.
	stub(t, &tunnelIsUp, func(string) bool { return false })
	stub(t, &tunnelDevice, func(string) string { return "" })
	stub(t, &tunnelReport, func(string) (tunnel.Status, error) { return tunnel.Status{}, nil })
	stub(t, &tunnelAddressPresent, func(string) bool { return false })
	return home
}

// converse scripts the terminal. A question is answered by the longest key it
// contains; an empty answer takes the question's default, as Enter would.
func converse(t *testing.T, replies map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(replies))
	for key := range replies {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	reply := func(question string) (string, bool) {
		for _, key := range keys {
			if strings.Contains(question, key) {
				return replies[key], true
			}
		}
		t.Errorf("nobody scripted an answer to %q", question)
		return "", false
	}
	stub(t, &ask, func(question, fallback string) string {
		if answer, ok := reply(question); ok && answer != "" {
			return answer
		}
		return fallback
	})
	stub(t, &askSecret, func(question string) string {
		answer, _ := reply(question)
		return answer
	})
	stub(t, &confirm, func(question string, fallback bool) bool {
		answer, ok := reply(question)
		if !ok || answer == "" {
			return fallback
		}
		return strings.HasPrefix(answer, "y")
	})
}

func saveProfile(t *testing.T, s setup.Settings) {
	t.Helper()
	if err := setup.SaveProfileSettings(s); err != nil {
		t.Fatalf("saving profile %s: %v", s.ProfileName, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

// --- a fake AWS ---------------------------------------------------------------
//
// Just enough of STS, SSM, CloudFormation, ELBv2, EC2 and Auto Scaling to
// answer what the commands ask, served on 127.0.0.1 and reached through
// AWS_ENDPOINT_URL, so the real SDK clients are what is exercised.

const (
	fakeAccount = "123456789012"
	fakeRegion  = "eu-west-1"
)

type fakeAWS struct {
	mu sync.Mutex

	arn             string
	parameters      map[string]string
	outputs         map[string]string
	stackParameters map[string]string
	regions         []string
	inService       bool

	// denied answers these actions, and every request signed with
	// deniedKey, with AccessDenied.
	denied    map[string]bool
	deniedKey string
	// identityLimit is how many GetCallerIdentity calls succeed; zero is
	// every one.
	identityLimit int

	calls        []string
	identities   int
	deregistered []string
	desired      int
}

func newFakeAWS(t *testing.T, home string) *fakeAWS {
	t.Helper()
	fake := &fakeAWS{
		arn:             "arn:aws:iam::" + fakeAccount + ":user/alice",
		parameters:      map[string]string{},
		outputs:         map[string]string{},
		stackParameters: map[string]string{},
		denied:          map[string]bool{},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	t.Setenv("AWS_ENDPOINT_URL", server.URL)

	directory := filepath.Join(home, ".aws")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := "[default]\naws_access_key_id = AKIDDEFAULT\naws_secret_access_key = secret\n\n" +
		"[noregion]\naws_access_key_id = AKIDNOREGION\naws_secret_access_key = secret\n"
	config := "[default]\nregion = " + fakeRegion + "\n\n[profile noregion]\noutput = json\n"
	if err := os.WriteFile(filepath.Join(directory, "credentials"), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return fake
}

func (f *fakeAWS) called(action string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call == action {
			return true
		}
	}
	return false
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()

	refused := f.deniedKey != "" && strings.Contains(r.Header.Get("Authorization"), "Credential="+f.deniedKey+"/")

	if target := r.Header.Get("X-Amz-Target"); target != "" {
		_, operation, _ := strings.Cut(target, ".")
		f.calls = append(f.calls, operation)
		f.serveSSM(w, operation, body, refused || f.denied[operation])
		return
	}

	form, _ := url.ParseQuery(string(body))
	action := form.Get("Action")
	f.calls = append(f.calls, action)
	if action == "GetCallerIdentity" {
		f.identities++
		refused = refused || (f.identityLimit > 0 && f.identities > f.identityLimit)
	}
	if refused || f.denied[action] {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code>`+
			`<Message>denied by the fake</Message></Error><RequestId>1</RequestId></ErrorResponse>`)
		return
	}

	w.Header().Set("Content-Type", "text/xml")
	switch action {
	case "GetCallerIdentity":
		fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>%s</Arn>`+
			`<UserId>AIDATEST</UserId><Account>%s</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`,
			f.arn, fakeAccount)
	case "DescribeStacks":
		var outputs, parameters strings.Builder
		for key, value := range f.outputs {
			fmt.Fprintf(&outputs, `<member><OutputKey>%s</OutputKey><OutputValue>%s</OutputValue></member>`, key, value)
		}
		for key, value := range f.stackParameters {
			fmt.Fprintf(&parameters, `<member><ParameterKey>%s</ParameterKey><ParameterValue>%s</ParameterValue></member>`, key, value)
		}
		fmt.Fprintf(w, `<DescribeStacksResponse><DescribeStacksResult><Stacks><member>`+
			`<StackName>%s</StackName><StackStatus>CREATE_COMPLETE</StackStatus>`+
			`<Outputs>%s</Outputs><Parameters>%s</Parameters>`+
			`</member></Stacks></DescribeStacksResult></DescribeStacksResponse>`,
			form.Get("StackName"), outputs.String(), parameters.String())
	case "DeregisterTargets":
		f.deregistered = append(f.deregistered,
			form.Get("TargetGroupArn")+" "+form.Get("Targets.member.1.Id")+":"+form.Get("Targets.member.1.Port"))
		fmt.Fprint(w, `<DeregisterTargetsResponse><DeregisterTargetsResult/></DeregisterTargetsResponse>`)
	case "DescribeRegions":
		var items strings.Builder
		for _, region := range f.regions {
			fmt.Fprintf(&items, `<item><regionName>%s</regionName></item>`, region)
		}
		fmt.Fprintf(w, `<DescribeRegionsResponse><requestId>1</requestId><regionInfo>%s</regionInfo></DescribeRegionsResponse>`,
			items.String())
	case "DescribeAutoScalingGroups":
		instances := ""
		if f.inService {
			instances = `<member><InstanceId>i-1</InstanceId><LifecycleState>InService</LifecycleState></member>`
		}
		fmt.Fprintf(w, `<DescribeAutoScalingGroupsResponse><DescribeAutoScalingGroupsResult><AutoScalingGroups><member>`+
			`<AutoScalingGroupName>%s</AutoScalingGroupName><DesiredCapacity>%d</DesiredCapacity><Instances>%s</Instances>`+
			`</member></AutoScalingGroups></DescribeAutoScalingGroupsResult></DescribeAutoScalingGroupsResponse>`,
			form.Get("AutoScalingGroupNames.member.1"), f.desired, instances)
	case "SetDesiredCapacity":
		fmt.Sscan(form.Get("DesiredCapacity"), &f.desired)
		fmt.Fprint(w, `<SetDesiredCapacityResponse></SetDesiredCapacityResponse>`)
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<ErrorResponse><Error><Code>InvalidAction</Code><Message>the fake has no %s</Message></Error></ErrorResponse>`, action)
	}
}

func (f *fakeAWS) serveSSM(w http.ResponseWriter, operation string, body []byte, refused bool) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	reject := func(kind string) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"__type":%q,"message":"from the fake"}`, kind)
	}
	if refused {
		reject("AccessDeniedException")
		return
	}
	var input struct{ Name, Value string }
	_ = json.Unmarshal(body, &input)
	switch operation {
	case "GetParameter":
		value, found := f.parameters[input.Name]
		if !found {
			reject("ParameterNotFound")
			return
		}
		encoded, _ := json.Marshal(map[string]any{
			"Parameter": map[string]any{"Name": input.Name, "Value": value, "Type": "String", "Version": 1},
		})
		w.Write(encoded)
	case "PutParameter":
		f.parameters[input.Name] = input.Value
		fmt.Fprint(w, `{"Version":2,"Tier":"Standard"}`)
	default:
		reject("InvalidAction")
	}
}
