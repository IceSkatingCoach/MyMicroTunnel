// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const packageBody = "the notarized package"

func newRelease(t *testing.T) string {
	t.Helper()
	t.Setenv("APPCAST_BASE_URL", "")
	t.Setenv("APPCAST_FEED_URL", "")
	root := newRepository(t, "1.4.0")
	writeFile(t, filepath.Join(root, "build", "MyMicroTunnel-1.4.0.pkg"), packageBody)
	return root
}

func caskAt(t *testing.T, root string) string {
	t.Helper()
	return readText(t, filepath.Join(root, "packaging", "homebrew", "mymicrotunnel.rb"))
}

func TestTheCaskDescribesTheBuiltPackageByItsOwnChecksum(t *testing.T) {
	root := newRelease(t)

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}

	sum := sha256.Sum256([]byte(packageBody))
	cask := caskAt(t, root)
	for _, want := range []string{
		`version "1.4.0"`,
		`sha256 "` + hex.EncodeToString(sum[:]) + `"`,
		`url "https://downloads.maragato.ca/releases/MyMicroTunnel-#{version}.pkg"`,
		`url "https://downloads.maragato.ca/appcast.xml"`,
		`homepage "https://mymicrotunnel.maragato.ca/"`,
		`pkg "MyMicroTunnel-#{version}.pkg"`,
	} {
		if !strings.Contains(cask, want) {
			t.Errorf("the cask is missing %s:\n%s", want, cask)
		}
	}
	if !strings.Contains(stdout, "homebrew-mymicrotunnel/Casks/mymicrotunnel.rb") {
		t.Errorf("the output does not say where the cask goes:\n%s", stdout)
	}
}

func TestTheURLsComeFromTheFlagsOrTheEnvironment(t *testing.T) {
	root := newRelease(t)
	t.Setenv("APPCAST_FEED_URL", "https://feed.example.com/appcast.xml")

	code, _, stderr := runMain(t,
		"--base-url", "https://cdn.example.com/releases/",
		"--homepage", "https://example.com/")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	cask := caskAt(t, root)
	for _, want := range []string{
		// The trailing slash is dropped, or brew would fetch from "releases//".
		`url "https://cdn.example.com/releases/MyMicroTunnel-#{version}.pkg"`,
		`url "https://feed.example.com/appcast.xml"`,
		`homepage "https://example.com/"`,
	} {
		if !strings.Contains(cask, want) {
			t.Errorf("the cask is missing %s:\n%s", want, cask)
		}
	}
}

func TestWithoutABuiltPackageNoCaskIsWritten(t *testing.T) {
	root := newRepository(t, "1.4.0")

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "make pkg-notarized") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "packaging", "homebrew", "mymicrotunnel.rb")); err == nil {
		t.Errorf("a cask was written for a package that does not exist")
	}
}

func TestCheckAsksTheDownloadURLWhetherTheReleaseIsPublished(t *testing.T) {
	var asked []string
	published := map[string]bool{"/releases/MyMicroTunnel-1.4.0.pkg": true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		if !published[r.URL.Path] {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()

	newRelease(t)
	code, stdout, stderr := runMain(t, "--check", "--base-url", server.URL+"/releases")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if len(asked) != 1 || asked[0] != "HEAD /releases/MyMicroTunnel-1.4.0.pkg" {
		t.Errorf("asked %v", asked)
	}
	if !strings.Contains(stdout, "is published") {
		t.Errorf("the output does not confirm publication:\n%s", stdout)
	}

	delete(published, "/releases/MyMicroTunnel-1.4.0.pkg")
	code, _, stderr = runMain(t, "--check", "--base-url", server.URL+"/releases")
	if code != 1 || !strings.Contains(stderr, "answers 403 Forbidden") || !strings.Contains(stderr, "make publish") {
		t.Errorf("an unpublished release exited %d with %q", code, stderr)
	}
}

func TestCheckFailsWhenTheDownloadHostIsUnreachable(t *testing.T) {
	newRelease(t)

	code, _, stderr := runMain(t, "--check", "--base-url", "http://127.0.0.1:1/releases")
	if code != 1 || !strings.Contains(stderr, "Could not reach http://127.0.0.1:1/releases/MyMicroTunnel-1.4.0.pkg") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestOutsideARepositoryItRefusesToGuess(t *testing.T) {
	t.Chdir(t.TempDir())

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "Not inside the repository") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestWithoutAVersionFileNothingIsWritten(t *testing.T) {
	newRepository(t, "")

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "VERSION") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestAPackagingPathThatIsAFileStopsTheCask(t *testing.T) {
	root := newRelease(t)
	writeFile(t, filepath.Join(root, "packaging"), "not a directory")

	if code, _, stderr := runMain(t); code != 1 || !strings.Contains(stderr, "packaging") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestEnvOrPrefersTheEnvironment(t *testing.T) {
	t.Setenv("CASK_TEST_VALUE", "")
	if got := envOr("CASK_TEST_VALUE", "fallback"); got != "fallback" {
		t.Errorf("an empty variable gave %q", got)
	}
	t.Setenv("CASK_TEST_VALUE", "set")
	if got := envOr("CASK_TEST_VALUE", "fallback"); got != "set" {
		t.Errorf("a set variable gave %q", got)
	}
}
