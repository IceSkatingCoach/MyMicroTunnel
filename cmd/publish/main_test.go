// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/infra"
)

// fakeAWS records what was asked of AWS, in order, and answers from its fields.
type fakeAWS struct {
	calls       []string
	outputs     map[string]string
	exists      bool
	zone        string
	failOn      string
	deployed    map[string]string
	deployedTpl string
}

func (f *fakeAWS) record(call string) error {
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.HasPrefix(call, f.failOn) {
		return errors.New("AccessDenied on " + call)
	}
	return nil
}

func (f *fakeAWS) Identity(ctx context.Context) (string, error) {
	return "arn:aws:iam::123456789012:user/releaser", f.record("Identity")
}

func (f *fakeAWS) FindHostedZone(ctx context.Context, domain string) (string, error) {
	return f.zone, f.record("FindHostedZone " + domain)
}

func (f *fakeAWS) DeployStack(ctx context.Context, name, templateBody string, parameters map[string]string, onProgress func(string)) error {
	f.deployed, f.deployedTpl = parameters, templateBody
	onProgress("AWS::S3::Bucket Feed CREATE_COMPLETE")
	return f.record("DeployStack " + name)
}

func (f *fakeAWS) StackOutputs(ctx context.Context, stackName string) (map[string]string, error) {
	return f.outputs, f.record("StackOutputs " + stackName)
}

func (f *fakeAWS) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	return f.exists, f.record("ObjectExists " + bucket + " " + key)
}

func (f *fakeAWS) Upload(ctx context.Context, bucket, key, path, contentType, cacheControl string) error {
	return f.record(fmt.Sprintf("Upload %s %s %s %s %q", bucket, key, filepath.Base(path), contentType, cacheControl))
}

func (f *fakeAWS) Invalidate(ctx context.Context, distributionID string, paths ...string) (string, error) {
	return "I2J3K4", f.record("Invalidate " + distributionID + " " + strings.Join(paths, " "))
}

func installAWS(t *testing.T, client *fakeAWS) *[]string {
	t.Helper()
	var loaded []string
	saved := loadProfile
	loadProfile = func(ctx context.Context, profile, region string) (feedClient, error) {
		loaded = append(loaded, profile+" "+region)
		return client, nil
	}
	t.Cleanup(func() { loadProfile = saved })
	return &loaded
}

// feedServer serves body as the public feed.
func feedServer(t *testing.T, body func() (int, string)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, text := body()
		w.WriteHeader(status)
		w.Write([]byte(text))
	}))
	t.Cleanup(server.Close)
	return server.URL + "/appcast.xml"
}

func fastFeed(t *testing.T, patience time.Duration) {
	t.Helper()
	savedPatience, savedInterval := feedPatience, feedRetryInterval
	feedPatience, feedRetryInterval = patience, time.Millisecond
	t.Cleanup(func() { feedPatience, feedRetryInterval = savedPatience, savedInterval })
}

const advertising = "<rss><sparkle:version>2.0.1</sparkle:version></rss>"

// newRelease lays out a built release and the stack that serves it.
func newRelease(t *testing.T, feedURL string) (string, *fakeAWS) {
	t.Helper()
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	root := newRepository(t, "2.0.1")
	for _, name := range []string{"MyMicroTunnel-2.0.1.zip", "MyMicroTunnel-2.0.1.pkg", "appcast.xml"} {
		writeFile(t, filepath.Join(root, "build", name), name)
	}
	fastFeed(t, time.Second)
	return root, &fakeAWS{outputs: map[string]string{
		"BucketName":     "feed-bucket",
		"DistributionId": "E123",
		"FeedUrl":        feedURL,
		"ArchiveBaseUrl": "https://downloads.example.com/releases",
	}}
}

func validatingGo(t *testing.T, output string, code int) *fakeTools {
	t.Helper()
	return installTools(t, func(name string, args []string) (string, int) { return output, code })
}

func TestAReleaseGoesUpArchiveFirstAndIsReadBack(t *testing.T) {
	url := feedServer(t, func() (int, string) { return http.StatusOK, advertising })
	root, aws := newRelease(t, url)
	loaded := installAWS(t, aws)
	tools := validatingGo(t, "", 0)

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}

	if !slices.Equal(*loaded, []string{"default us-east-1"}) {
		t.Errorf("credentials loaded as %v", *loaded)
	}
	want := []string{
		"Identity",
		"StackOutputs xprem-vpn-updates",
		"ObjectExists feed-bucket releases/MyMicroTunnel-2.0.1.zip",
		`Upload feed-bucket releases/MyMicroTunnel-2.0.1.zip MyMicroTunnel-2.0.1.zip application/zip "public, max-age=31536000, immutable"`,
		`Upload feed-bucket releases/MyMicroTunnel-2.0.1.pkg MyMicroTunnel-2.0.1.pkg application/octet-stream "public, max-age=31536000, immutable"`,
		`Upload feed-bucket MyMicroTunnel.pkg MyMicroTunnel-2.0.1.pkg application/octet-stream "public, max-age=300"`,
		`Upload feed-bucket appcast.xml appcast.xml application/xml "public, max-age=300"`,
		"Invalidate E123 /appcast.xml /MyMicroTunnel.pkg",
	}
	if !slices.Equal(aws.calls, want) {
		t.Errorf("AWS was asked\n%s\nwant\n%s", strings.Join(aws.calls, "\n"), strings.Join(want, "\n"))
	}

	if len(tools.calls) != 1 || tools.calls[0].line() != "go run ./cmd/appcast --validate" || tools.calls[0].cmd.Dir != root {
		t.Errorf("the release was not validated from the checkout: %v", tools.lines())
	}
	if !strings.Contains(stdout, "It advertises 2.0.1") || !strings.Contains(stdout, "2.0.1 is published at "+url) {
		t.Errorf("the output does not confirm the feed:\n%s", stdout)
	}
}

func TestAReleaseThatDoesNotValidateIsNeverUploaded(t *testing.T) {
	_, aws := newRelease(t, "http://127.0.0.1:1/appcast.xml")
	installAWS(t, aws)
	validatingGo(t, "the feed advertises 2.0.0 but the build is 2.0.1", 1)

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "the feed advertises 2.0.0 but the build is 2.0.1") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	for _, call := range aws.calls {
		if strings.HasPrefix(call, "Upload") || strings.HasPrefix(call, "ObjectExists") {
			t.Errorf("touched the bucket after a failed validation: %s", call)
		}
	}
}

func TestAPublishedVersionIsNotOverwrittenWithoutForce(t *testing.T) {
	url := feedServer(t, func() (int, string) { return http.StatusOK, advertising })
	_, aws := newRelease(t, url)
	aws.exists = true
	installAWS(t, aws)
	validatingGo(t, "", 0)

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "s3://feed-bucket/releases/MyMicroTunnel-2.0.1.zip is already published") {
		t.Fatalf("exited %d with %q", code, stderr)
	}
	if slices.ContainsFunc(aws.calls, func(call string) bool { return strings.HasPrefix(call, "Upload") }) {
		t.Errorf("uploaded over a published version: %v", aws.calls)
	}

	aws.calls = nil
	if code, _, stderr := runMain(t, "--force"); code != 0 {
		t.Fatalf("--force exited %d:\n%s", code, stderr)
	}
	if !slices.ContainsFunc(aws.calls, func(call string) bool { return strings.HasPrefix(call, "Upload") }) {
		t.Errorf("--force uploaded nothing: %v", aws.calls)
	}
}

func TestAMissingBuildFileStopsThePublishBeforeAnything(t *testing.T) {
	for _, missing := range []string{"MyMicroTunnel-2.0.1.zip", "MyMicroTunnel-2.0.1.pkg", "appcast.xml"} {
		t.Run(missing, func(t *testing.T) {
			root, aws := newRelease(t, "https://downloads.example.com/appcast.xml")
			if err := os.Remove(filepath.Join(root, "build", missing)); err != nil {
				t.Fatal(err)
			}
			installAWS(t, aws)
			tools := validatingGo(t, "", 0)

			code, _, stderr := runMain(t)
			if code != 1 || !strings.Contains(stderr, missing+" is missing") {
				t.Errorf("exited %d with %q", code, stderr)
			}
			if !strings.Contains(stderr, "APPCAST_FEED_URL=https://downloads.example.com/appcast.xml") {
				t.Errorf("the instructions do not carry the feed URL:\n%s", stderr)
			}
			if len(tools.calls) != 0 {
				t.Errorf("validated an incomplete release: %v", tools.lines())
			}
		})
	}
}

func TestAStackThatIsNotTheFeedIsRefused(t *testing.T) {
	_, aws := newRelease(t, "")
	installAWS(t, aws)
	validatingGo(t, "", 0)

	code, _, stderr := runMain(t, "--stack", "something-else")
	if code != 1 || !strings.Contains(stderr, "something-else does not look like the update-feed stack") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestEachAWSFailureStopsThePublishWhereItHappened(t *testing.T) {
	for _, test := range []struct {
		failOn string
		want   string
	}{
		{"Identity", "Those credentials do not work"},
		{"StackOutputs", "Deploy the feed's infrastructure first"},
		{"ObjectExists", "Could not check releases/MyMicroTunnel-2.0.1.zip"},
		{"Upload feed-bucket releases/MyMicroTunnel-2.0.1.zip", "AccessDenied"},
		{"Upload feed-bucket releases/MyMicroTunnel-2.0.1.pkg", "AccessDenied"},
		{"Upload feed-bucket MyMicroTunnel.pkg", "AccessDenied"},
		{"Upload feed-bucket appcast.xml", "AccessDenied"},
		{"Invalidate", "AccessDenied"},
	} {
		t.Run(test.failOn, func(t *testing.T) {
			_, aws := newRelease(t, "http://127.0.0.1:1/appcast.xml")
			aws.failOn = test.failOn
			installAWS(t, aws)
			validatingGo(t, "", 0)

			code, _, stderr := runMain(t)
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q, want %q", code, stderr, test.want)
			}
			if last := aws.calls[len(aws.calls)-1]; !strings.HasPrefix(last, test.failOn) {
				t.Errorf("carried on after %s failed: %v", test.failOn, aws.calls)
			}
		})
	}
}

func TestCredentialsThatDoNotLoadStopEverything(t *testing.T) {
	newRelease(t, "")
	t.Setenv("AWS_PROFILE", "releases")
	t.Setenv("AWS_REGION", "eu-west-1")
	var loaded string
	saved := loadProfile
	loadProfile = func(ctx context.Context, profile, region string) (feedClient, error) {
		loaded = profile + " " + region
		return nil, errors.New("no such profile")
	}
	t.Cleanup(func() { loadProfile = saved })

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "Could not load AWS credentials: no such profile") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if loaded != "releases eu-west-1" {
		t.Errorf("the environment's profile and region were not used: %q", loaded)
	}
}

func TestTheFeedIsRetriedUntilTheCacheCatchesUp(t *testing.T) {
	var requests atomic.Int32
	url := feedServer(t, func() (int, string) {
		switch requests.Add(1) {
		case 1:
			return http.StatusServiceUnavailable, ""
		case 2:
			return http.StatusOK, "<rss><sparkle:version>2.0.0</sparkle:version></rss>"
		}
		return http.StatusOK, advertising
	})
	fastFeed(t, time.Minute)

	code, stdout, _ := capture(t, func() { verify(url, "2.0.1") })
	if code != 0 || requests.Load() != 3 {
		t.Fatalf("exited %d after %d requests", code, requests.Load())
	}
	if !strings.Contains(stdout, "returned 503 Service Unavailable") || !strings.Contains(stdout, "still serving the previous feed") {
		t.Errorf("the waiting was not explained:\n%s", stdout)
	}
}

func TestAFeedThatNeverCatchesUpIsAFailure(t *testing.T) {
	url := feedServer(t, func() (int, string) { return http.StatusOK, "<rss/>" })
	fastFeed(t, 0)

	code, _, stderr := capture(t, func() { verify(url, "2.0.1") })
	if code != 1 || !strings.Contains(stderr, "still does not advertise 2.0.1") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestSetupDeploysTheFeedStackIntoTheDiscoveredZone(t *testing.T) {
	aws := &fakeAWS{zone: "Z0123456789", outputs: map[string]string{
		"FeedUrl":        "https://downloads.example.com/appcast.xml",
		"ArchiveBaseUrl": "https://downloads.example.com/releases",
	}}
	installAWS(t, aws)

	code, stdout, stderr := runMain(t, "--setup", "--bucket", "feed-bucket", "--domain", "downloads.example.com",
		"--profile", "admin", "--region", "us-east-1")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	want := []string{"Identity", "FindHostedZone downloads.example.com", "DeployStack xprem-vpn-updates", "StackOutputs xprem-vpn-updates"}
	if !slices.Equal(aws.calls, want) {
		t.Errorf("AWS was asked %v, want %v", aws.calls, want)
	}
	wantParameters := map[string]string{
		"BucketName":   "feed-bucket",
		"DomainName":   "downloads.example.com",
		"HostedZoneId": "Z0123456789",
	}
	if fmt.Sprint(aws.deployed) != fmt.Sprint(wantParameters) {
		t.Errorf("deployed with %v, want %v", aws.deployed, wantParameters)
	}
	if aws.deployedTpl != infra.UpdatesTemplate {
		t.Errorf("deployed a template other than infra.UpdatesTemplate")
	}
	for _, want := range []string{
		"AWS::S3::Bucket Feed CREATE_COMPLETE",
		"APPCAST_FEED_URL=https://downloads.example.com/appcast.xml",
		"make appcast APPCAST_BASE_URL=https://downloads.example.com/releases",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not say %q:\n%s", want, stdout)
		}
	}
}

func TestSetupWithAGivenZoneDoesNotLookOneUp(t *testing.T) {
	aws := &fakeAWS{outputs: map[string]string{}}
	installAWS(t, aws)

	if code, _, stderr := runMain(t, "--setup", "--bucket", "b", "--domain", "d.example.com", "--hosted-zone", "ZGIVEN"); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if slices.ContainsFunc(aws.calls, func(call string) bool { return strings.HasPrefix(call, "FindHostedZone") }) {
		t.Errorf("looked up a zone it was given: %v", aws.calls)
	}
	if aws.deployed["HostedZoneId"] != "ZGIVEN" {
		t.Errorf("deployed into zone %q", aws.deployed["HostedZoneId"])
	}
}

func TestSetupWithoutADomainWarnsThatTheNameIsPermanent(t *testing.T) {
	aws := &fakeAWS{outputs: map[string]string{}}
	installAWS(t, aws)

	code, stdout, stderr := runMain(t, "--setup", "--bucket", "b")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "*.cloudfront.net") || !strings.Contains(stdout, "Use a domain you own") {
		t.Errorf("no warning about the cloudfront.net name:\n%s", stdout)
	}
}

func TestSetupRefusesToPickABucketName(t *testing.T) {
	aws := &fakeAWS{}
	installAWS(t, aws)

	code, _, stderr := runMain(t, "--setup")
	if code != 1 || !strings.Contains(stderr, "--setup needs --bucket") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if slices.ContainsFunc(aws.calls, func(call string) bool { return strings.HasPrefix(call, "DeployStack") }) {
		t.Errorf("deployed without a bucket: %v", aws.calls)
	}
}

func TestSetupFailuresStopWhereTheyHappen(t *testing.T) {
	for _, test := range []struct {
		failOn string
		want   string
	}{
		{"FindHostedZone", "AccessDenied on FindHostedZone"},
		{"DeployStack", "The deployment failed"},
		{"StackOutputs", "Could not read the stack outputs"},
	} {
		t.Run(test.failOn, func(t *testing.T) {
			aws := &fakeAWS{failOn: test.failOn}
			installAWS(t, aws)

			code, _, stderr := runMain(t, "--setup", "--bucket", "b", "--domain", "d.example.com")
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q, want %q", code, stderr, test.want)
			}
		})
	}
}

func TestPublishingNeedsTheCheckoutAndItsVersion(t *testing.T) {
	aws := &fakeAWS{outputs: map[string]string{"BucketName": "b", "DistributionId": "E", "FeedUrl": "u"}}
	installAWS(t, aws)

	t.Run("outside a repository", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if code, _, stderr := runMain(t); code != 1 || !strings.Contains(stderr, "Could not find the repository root.") {
			t.Errorf("exited %d with %q", code, stderr)
		}
	})
	t.Run("without VERSION", func(t *testing.T) {
		newRepository(t, "")
		if code, _, stderr := runMain(t); code != 1 || !strings.Contains(stderr, "No VERSION file") {
			t.Errorf("exited %d with %q", code, stderr)
		}
	})
}

func TestARunThatCannotStartIsAFailure(t *testing.T) {
	if _, code := runIn(t.TempDir(), filepath.Join(t.TempDir(), "no-such-tool")); code != -1 {
		t.Errorf("a missing tool gave code %d, want -1", code)
	}
}
