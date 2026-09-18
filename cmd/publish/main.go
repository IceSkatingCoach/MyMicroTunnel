// SPDX-License-Identifier: GPL-3.0-or-later

// Command publish deploys the update-feed stack and puts a release on it.
//
//	go run ./cmd/publish --setup --bucket xpremvpn-downloads --domain downloads.example.com
//	go run ./cmd/publish
//
// --setup deploys or updates the CloudFront and S3 stack, and prints the two
// values a release build needs: the feed URL that gets compiled into the app,
// and the base URL the appcast points its enclosure at. It is run once, and
// then only when that infrastructure changes.
//
// Without --setup it publishes: uploads the archive and the appcast that
// `make appcast` produced, invalidates the cached feed, and then fetches the
// feed back over the public URL to prove that what a user's copy of the app
// will read is what was just written.
//
// ORDER MATTERS
// -------------
// The archive goes up before the appcast, always. A feed naming an archive that
// is not there yet is a feed every installed copy will try, and fail, to update
// from — for however long the gap lasts. The other order is merely slower.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/IceSkatingCoach/wiregard_mini_vpn/infra"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/awsops"
)

const defaultStackName = "xprem-vpn-updates"

func main() {
	setup := flag.Bool("setup", false, "deploy or update the feed's own infrastructure and exit")
	stackName := flag.String("stack", defaultStackName, "CloudFormation stack holding the feed")
	profile := flag.String("profile", envOr("AWS_PROFILE", "default"), "AWS profile")
	region := flag.String("region", envOr("AWS_REGION", "us-east-1"),
		"AWS region; CloudFront reads certificates only from us-east-1")
	bucket := flag.String("bucket", "", "bucket name, for --setup")
	domain := flag.String("domain", "", "hostname to serve the feed from, for --setup")
	hostedZone := flag.String("hosted-zone", "", "hosted zone for --domain; discovered when omitted")
	force := flag.Bool("force", false, "overwrite an archive that is already published")
	flag.Parse()

	ctx := context.Background()
	client, err := awsops.LoadProfile(ctx, *profile, *region)
	if err != nil {
		fail("Could not load AWS credentials: %v", err)
	}
	identity, err := client.Identity(ctx)
	if err != nil {
		fail("Those credentials do not work: %v", err)
	}
	done("Authenticated as %s", identity)

	if *setup {
		deployFeedStack(ctx, client, *stackName, *bucket, *domain, *hostedZone)
		return
	}
	publish(ctx, client, *stackName, *force)
}

// --- the feed's own infrastructure -----------------------------------------

func deployFeedStack(ctx context.Context, client *awsops.Client, stackName, bucket, domain, hostedZone string) {
	if bucket == "" {
		fail("--setup needs --bucket. S3 bucket names are global, so there is no\n" +
			"  default that is safe to pick on your behalf.")
	}
	if domain != "" && hostedZone == "" {
		step("Finding the hosted zone for %s", domain)
		zone, err := client.FindHostedZone(ctx, domain)
		if err != nil {
			fail("%v", err)
		}
		hostedZone = zone
		done("%s", hostedZone)
	}
	if domain == "" {
		warn("No --domain, so the feed will live at a *.cloudfront.net name.")
		warn("That name is compiled into every app you ship. Use a domain you own.")
	}

	step("Deploying %s", stackName)
	err := client.DeployStack(ctx, stackName, infra.UpdatesTemplate, map[string]string{
		"BucketName":   bucket,
		"DomainName":   domain,
		"HostedZoneId": hostedZone,
	}, func(resource string) { info("%s", resource) })
	if err != nil {
		fail("The deployment failed: %v", err)
	}
	done("Deployed")

	outputs, err := client.StackOutputs(ctx, stackName)
	if err != nil {
		fail("Could not read the stack outputs: %v", err)
	}

	fmt.Printf("\n✓ The feed is ready.\n\n")
	fmt.Printf("  Build releases with:\n\n")
	fmt.Printf("      make pkg-notarized PROFILE=<notary-profile> \\\n")
	fmt.Printf("           APPCAST_FEED_URL=%s \\\n", outputs["FeedUrl"])
	fmt.Printf("           SPARKLE_PUBLIC_KEY=<from `make sparkle-keys`>\n")
	fmt.Printf("      make appcast APPCAST_BASE_URL=%s\n", outputs["ArchiveBaseUrl"])
	fmt.Printf("      make publish\n\n")
	fmt.Printf("  %s is baked into every app you ship. It cannot be\n", outputs["FeedUrl"])
	fmt.Printf("  changed later without orphaning every installed copy.\n")
}

// --- publishing a release --------------------------------------------------

func publish(ctx context.Context, client *awsops.Client, stackName string, force bool) {
	outputs, err := client.StackOutputs(ctx, stackName)
	if err != nil {
		fail("Could not read %s: %v\n\n"+
			"  Deploy the feed's infrastructure first:\n\n"+
			"      go run ./cmd/publish --setup --bucket <name> --domain <hostname>",
			stackName, err)
	}
	bucket := outputs["BucketName"]
	distribution := outputs["DistributionId"]
	feedURL := outputs["FeedUrl"]
	if bucket == "" || distribution == "" || feedURL == "" {
		fail("%s does not look like the update-feed stack.", stackName)
	}

	root := repoRoot()
	version := readVersion(root)
	buildDir := filepath.Join(root, "build")
	archivePath := filepath.Join(buildDir, fmt.Sprintf("XpremVpn-%s.zip", version))
	feedPath := filepath.Join(buildDir, "appcast.xml")

	for _, required := range []string{archivePath, feedPath} {
		if _, err := os.Stat(required); err != nil {
			fail("%s is missing. Build the release first:\n\n"+
				"      make pkg-notarized PROFILE=<profile> APPCAST_FEED_URL=%s SPARKLE_PUBLIC_KEY=<key>\n"+
				"      make appcast APPCAST_BASE_URL=%s",
				filepath.Base(required), feedURL, outputs["ArchiveBaseUrl"])
		}
	}

	// The appcast is checked before anything is uploaded, so a feed that would
	// be rejected by every installed copy never reaches the bucket.
	step("Checking the release before uploading it")
	if output, code := runGo(root, "run", "./cmd/appcast", "--validate"); code != 0 {
		fail("The release does not validate, so it was not published:\n\n%s", output)
	}
	done("The feed and the archive agree")

	archiveKey := "releases/" + filepath.Base(archivePath)
	published, err := client.ObjectExists(ctx, bucket, archiveKey)
	if err != nil {
		fail("Could not check %s: %v", archiveKey, err)
	}
	if published && !force {
		fail("s3://%s/%s is already published.\n\n"+
			"  Releasing a different build under a version that is already out means\n"+
			"  two machines can have different software claiming the same version.\n"+
			"  Raise VERSION, or pass --force if this is genuinely the same build.",
			bucket, archiveKey)
	}

	// Archive first. A feed naming an archive that is not there yet is a feed
	// every installed copy fails to update from.
	step("Uploading %s", filepath.Base(archivePath))
	if err := client.Upload(ctx, bucket, archiveKey, archivePath,
		"application/zip", "public, max-age=31536000, immutable"); err != nil {
		fail("%v", err)
	}
	done("s3://%s/%s", bucket, archiveKey)

	step("Uploading the appcast")
	if err := client.Upload(ctx, bucket, "appcast.xml", feedPath,
		"application/xml", "public, max-age=300"); err != nil {
		fail("%v", err)
	}
	done("s3://%s/appcast.xml", bucket)

	step("Invalidating the cached feed")
	invalidation, err := client.Invalidate(ctx, distribution, "/appcast.xml")
	if err != nil {
		fail("%v", err)
	}
	done("%s", invalidation)

	verify(feedURL, version)

	fmt.Printf("\n✓ %s is published at %s\n", version, feedURL)
}

// verify reads the feed back over the public URL. Everything before this proves
// what was uploaded; only this proves what will be served.
func verify(feedURL, version string) {
	step("Reading the feed back from %s", feedURL)

	// The invalidation is not instant, so a stale feed for a few seconds is
	// expected rather than a failure.
	deadline := time.Now().Add(2 * time.Minute)
	client := &http.Client{Timeout: 20 * time.Second}

	for attempt := 0; ; attempt++ {
		body, err := fetch(client, feedURL)
		switch {
		case err != nil:
			info("%v", err)
		case strings.Contains(body, "<sparkle:version>"+version+"</sparkle:version>"):
			done("It advertises %s", version)
			return
		default:
			info("still serving the previous feed")
		}

		if time.Now().After(deadline) {
			fail("The feed at %s still does not advertise %s.\n\n"+
				"  The upload succeeded, so this is the cache or the distribution.\n"+
				"  Check the invalidation in the CloudFront console.", feedURL, version)
		}
		time.Sleep(10 * time.Second)
	}
}

func fetch(client *http.Client, url string) (string, error) {
	response, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned %s", url, response.Status)
	}
	return string(body), nil
}

// --- plumbing --------------------------------------------------------------

func runGo(directory string, args ...string) (string, int) {
	return runIn(directory, "go", args...)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func readVersion(root string) string {
	content, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		fail("No VERSION file at the repository root: %v", err)
	}
	return strings.TrimSpace(string(content))
}

func repoRoot() string {
	working, err := os.Getwd()
	if err != nil {
		fail("%v", err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := os.Stat(filepath.Join(working, "go.mod")); err == nil {
			return working
		}
		working = filepath.Dir(working)
	}
	fail("Could not find the repository root.")
	return ""
}

func step(format string, args ...any) { fmt.Printf("\n▸ "+format+"\n", args...) }
func done(format string, args ...any) { fmt.Printf("  ✓ "+format+"\n", args...) }
func warn(format string, args ...any) { fmt.Printf("  ! "+format+"\n", args...) }
func info(format string, args ...any) { fmt.Printf("  "+format+"\n", args...) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n✗ "+format+"\n", args...)
	os.Exit(1)
}

// runIn runs a command in the checkout and returns its combined output, so a
// failure can be shown in full rather than summarised.
func runIn(directory, name string, args ...string) (string, int) {
	command := exec.Command(name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		if code = command.ProcessState.ExitCode(); code == 0 {
			code = -1
		}
	}
	return strings.TrimSpace(string(output)), code
}
