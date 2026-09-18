// SPDX-License-Identifier: GPL-3.0-or-later
package infra

import (
	"regexp"
	"strings"
	"testing"
)

func TestUpdatesTemplateIsEmbedded(t *testing.T) {
	if !strings.Contains(UpdatesTemplate, "\nAWSTemplateFormatVersion") {
		t.Fatal("the embedded updates template does not declare AWSTemplateFormatVersion")
	}
	if !strings.Contains(UpdatesTemplate, "\nResources:") {
		t.Fatal("the embedded updates template has no Resources section")
	}
}

// This stack is the vendor's and is deployed by hand, but a template that names
// one account's resources is still a template that cannot be handed to anyone.
func TestUpdatesTemplateCarriesNoAccountSpecificIdentifiers(t *testing.T) {
	forbidden := regexp.MustCompile(`(vpc-[0-9a-f]{8,}|subnet-[0-9a-f]{8,}|\b\d{12}\b)`)
	for index, line := range strings.Split(UpdatesTemplate, "\n") {
		// Z2FDTNDATAQYW2 is CloudFront's own fixed hosted zone, the same for
		// every account, and is not an identifier belonging to anyone.
		if strings.Contains(line, "Z2FDTNDATAQYW2") {
			continue
		}
		if match := forbidden.FindString(line); match != "" {
			t.Errorf("line %d names %s:\n%s", index+1, match, line)
		}
	}
}

// Nothing in the bucket is reachable except through the distribution. The
// archives are a public download, but they are public *through CloudFront*, and
// a bucket that is also directly readable is one that can be enumerated.
func TestUpdatesBucketIsNotPublic(t *testing.T) {
	for _, setting := range []string{
		"BlockPublicAcls: true",
		"IgnorePublicAcls: true",
		"BlockPublicPolicy: true",
		"RestrictPublicBuckets: true",
	} {
		if !strings.Contains(UpdatesTemplate, setting) {
			t.Errorf("the bucket does not set %s", setting)
		}
	}
	if !strings.Contains(UpdatesTemplate, "aws:SecureTransport") {
		t.Error("the bucket policy does not refuse plain HTTP")
	}
	if !strings.Contains(UpdatesTemplate, "OnlyThisDistributionMayRead") {
		t.Error("the bucket policy does not restrict reads to this distribution")
	}
}

// The archives every installed copy downloads from outlive any particular
// stack, and an app that has not been opened in a year still asks for the
// version its feed named at the time.
func TestUpdatesBucketSurvivesTheStack(t *testing.T) {
	if !strings.Contains(UpdatesTemplate, "DeletionPolicy: Retain") {
		t.Error("the bucket is deleted with the stack")
	}
	if !strings.Contains(UpdatesTemplate, "Status: Enabled") {
		t.Error("the bucket is not versioned, so overwriting a published archive is final")
	}
}

// A cached feed delays every update by however long the cache lasts, and an
// archive never changes because its name carries the version. Getting these two
// the wrong way round is slow in one direction and wrong in the other.
func TestUpdatesCachesTheFeedBrieflyAndTheArchivesForever(t *testing.T) {
	feed := section(UpdatesTemplate, "FeedCachePolicy", "ArchiveCachePolicy")
	if !strings.Contains(feed, "MaxTTL: 300") {
		t.Errorf("the feed is not capped at five minutes:\n%s", feed)
	}

	archives := section(UpdatesTemplate, "ArchiveCachePolicy", "Distribution:")
	if !strings.Contains(archives, "MinTTL: 31536000") {
		t.Errorf("the archives are not cached for a year:\n%s", archives)
	}
}

func TestUpdatesRedirectsToHttps(t *testing.T) {
	// Sparkle will not read a feed over plain HTTP.
	if strings.Count(UpdatesTemplate, "ViewerProtocolPolicy: redirect-to-https") != 2 {
		t.Error("not every cache behaviour redirects to HTTPS")
	}
}

func TestUpdatesTemplateReportsWhatAReleaseNeeds(t *testing.T) {
	// cmd/publish reads all four by name; a rename here is a release that
	// stops halfway with a stack that "does not look like the update-feed
	// stack".
	for _, output := range []string{"FeedUrl", "ArchiveBaseUrl", "BucketName", "DistributionId"} {
		if !strings.Contains(UpdatesTemplate, "\n  "+output+":") {
			t.Errorf("the template has no %s output", output)
		}
	}
}

// section returns the text between two markers, so a check can be aimed at one
// resource rather than at the whole file.
func section(text, from, to string) string {
	start := strings.Index(text, from)
	if start < 0 {
		return ""
	}
	rest := text[start:]
	if end := strings.Index(rest[len(from):], to); end >= 0 {
		return rest[:len(from)+end]
	}
	return rest
}
