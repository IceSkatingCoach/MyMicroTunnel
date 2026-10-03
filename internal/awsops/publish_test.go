// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestUploadSendsTheFileWithItsHeaders(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("s3 PUT /releases/appcast.xml", awsReply{Header: map[string]string{"ETag": `"e"`}})
	path := filepath.Join(t.TempDir(), "appcast.xml")
	if err := os.WriteFile(path, []byte("<rss/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := fake.client().Upload(context.Background(), "releases", "appcast.xml", path, "application/xml", "max-age=60"); err != nil {
		t.Fatal(err)
	}
	calls := fake.made("s3 PUT /releases/appcast.xml?x-id=PutObject")
	if len(calls) != 1 {
		t.Fatalf("operations: %v", fake.operations())
	}
	call := calls[0]
	if call.Body != "<rss/>" || call.Header.Get("Content-Type") != "application/xml" ||
		call.Header.Get("Cache-Control") != "max-age=60" || call.Header.Get("Content-Length") != "6" {
		t.Errorf("unexpected upload: %q %v", call.Body, call.Header)
	}
}

func TestUploadNamesWhatFailed(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("s3 PUT /releases/a.zip", awsReply{Status: http.StatusForbidden,
		Body: "<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>"})
	client := fake.client()
	path := filepath.Join(t.TempDir(), "a.zip")
	if err := os.WriteFile(path, []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := client.Upload(context.Background(), "releases", "a.zip", path, "application/zip", "immutable")
	if err == nil || !strings.Contains(err.Error(), "uploading a.zip to s3://releases/a.zip") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("got %v", err)
	}

	if err := client.Upload(context.Background(), "releases", "a.zip", filepath.Join(t.TempDir(), "missing"), "", ""); !os.IsNotExist(err) {
		t.Errorf("a missing file: got %v", err)
	}
}

func TestObjectExistsTellsMissingFromUnreadable(t *testing.T) {
	fake := newFakeAWS(t)
	fake.sequence("s3 HEAD /releases/a.zip",
		awsReply{Header: map[string]string{"Content-Length": "3"}},
		awsReply{Status: http.StatusNotFound},
		awsReply{Status: http.StatusForbidden})
	client := fake.client()

	if exists, err := client.ObjectExists(context.Background(), "releases", "a.zip"); err != nil || !exists {
		t.Errorf("present: got %v, %v", exists, err)
	}
	if exists, err := client.ObjectExists(context.Background(), "releases", "a.zip"); err != nil || exists {
		t.Errorf("missing: got %v, %v", exists, err)
	}
	// Forbidden is not "absent": treating it so would let a release overwrite
	// an archive a published feed already points at.
	if _, err := client.ObjectExists(context.Background(), "releases", "a.zip"); err == nil {
		t.Error("a refused HEAD was reported as a missing object")
	}
}

func TestIsNotFoundRecognisesBothShapes(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("s3 GET /releases/a.zip", awsReply{Status: http.StatusNotFound,
		Body: "<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>"})

	_, err := fake.client().S3.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("releases"),
		Key:    aws.String("a.zip"),
	})
	if !isNotFound(err) {
		t.Errorf("NoSuchKey is not recognised: %v", err)
	}
	if isNotFound(os.ErrNotExist) {
		t.Error("an unrelated error was taken for a missing object")
	}
}

const invalidationXML = `<?xml version="1.0"?><Invalidation xmlns="http://cloudfront.amazonaws.com/doc/2020-05-31/">` +
	`<Id>I123</Id><Status>InProgress</Status><CreateTime>2026-01-01T00:00:00Z</CreateTime>` +
	`<InvalidationBatch><Paths><Quantity>1</Quantity><Items><Path>/appcast.xml</Path></Items></Paths><CallerReference>r</CallerReference></InvalidationBatch>` +
	`</Invalidation>`

func TestInvalidateDefaultsToTheFeed(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudfront POST /2020-05-31/distribution/E1/invalidation", awsReply{Status: http.StatusCreated, Body: invalidationXML})
	client := fake.client()

	id, err := client.Invalidate(context.Background(), "E1")
	if err != nil || id != "I123" {
		t.Fatalf("got %q, %v", id, err)
	}
	if _, err := client.Invalidate(context.Background(), "E1", "/a", "/b"); err != nil {
		t.Fatal(err)
	}

	calls := fake.made("cloudfront POST /2020-05-31/distribution/E1/invalidation")
	if len(calls) != 2 {
		t.Fatalf("operations: %v", fake.operations())
	}
	if !strings.Contains(calls[0].Body, "<Quantity>1</Quantity>") || !strings.Contains(calls[0].Body, "<Path>/appcast.xml</Path>") {
		t.Errorf("the default invalidation is not the feed:\n%s", calls[0].Body)
	}
	if !strings.Contains(calls[1].Body, "<Quantity>2</Quantity>") || !strings.Contains(calls[1].Body, "<Path>/b</Path>") {
		t.Errorf("the paths asked for were not sent:\n%s", calls[1].Body)
	}
	// A repeated reference is answered with the earlier invalidation instead
	// of a new one.
	reference := func(body string) string {
		_, after, _ := strings.Cut(body, "<CallerReference>")
		value, _, _ := strings.Cut(after, "</CallerReference>")
		return value
	}
	if first, second := reference(calls[0].Body), reference(calls[1].Body); first == "" || first == second {
		t.Errorf("caller references %q and %q are not unique", first, second)
	}
}

func TestInvalidateNamesTheDistribution(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("cloudfront POST /2020-05-31/distribution/E1/invalidation",
		queryError(http.StatusNotFound, "NoSuchDistribution", "The specified distribution does not exist."))

	_, err := fake.client().Invalidate(context.Background(), "E1")
	if err == nil || !strings.Contains(err.Error(), "invalidating [/appcast.xml] on E1") {
		t.Errorf("got %v", err)
	}
}
