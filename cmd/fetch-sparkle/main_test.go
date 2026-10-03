// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

const archiveBody = "pretend this is Sparkle-2.10.0.tar.xz"

// What the release tarball unpacks to, including the parts that must be left
// behind.
var tarball = map[string]string{
	"Sparkle.framework/Versions/B/Sparkle":     "framework binary",
	"bin/sign_update":                          "signer",
	"bin/generate_keys":                        "key generator",
	"LICENSE":                                  "Sparkle licence",
	"Sparkle Test App.app/Contents/Info.plist": "test app",
	"Symbols/Sparkle.framework.dSYM/Contents":  "debug symbols",
}

func checksumOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// serveArchive points the pin at a local server holding body, and counts the
// downloads.
func serveArchive(t *testing.T, status int, body string) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	savedURL, savedSum := archiveURL, archiveSHA256
	archiveURL = server.URL + "/Sparkle-2.10.0.tar.xz"
	archiveSHA256 = checksumOf(archiveBody)
	t.Cleanup(func() { archiveURL, archiveSHA256 = savedURL, savedSum })
	t.Setenv("TMPDIR", t.TempDir())
	return &requests
}

// unpackingTools stands in for tar and cp, doing what they would to the files.
func unpackingTools(t *testing.T, contents map[string]string, failing string) *fakeTools {
	t.Helper()
	return installTools(t, func(name string, args []string) (string, int) {
		if name == failing {
			return "", 1
		}
		switch name {
		case "tar":
			into := args[slices.Index(args, "-C")+1]
			for relative, content := range contents {
				writeFile(t, filepath.Join(into, relative), content)
			}
		case "cp":
			source, destination := args[len(args)-2], args[len(args)-1]
			if args[0] == "-R" {
				if err := os.CopyFS(destination, os.DirFS(source)); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, destination, readText(t, source))
			}
		}
		return "", 0
	})
}

func TestThePinNamesTheVersionItClaims(t *testing.T) {
	if archiveURL != sparkleURL || archiveSHA256 != sparkleSHA256 {
		t.Fatalf("the build does not download the pinned release")
	}
	if !strings.HasSuffix(sparkleURL, "/"+sparkleVersion+"/Sparkle-"+sparkleVersion+".tar.xz") {
		t.Errorf("%s is not the %s release", sparkleURL, sparkleVersion)
	}
	if len(sparkleSHA256) != 64 {
		t.Errorf("the pinned checksum %q is not a sha256", sparkleSHA256)
	}
}

func TestAVerifiedDownloadInstallsOnlyTheFrameworkAndItsTools(t *testing.T) {
	root := newRepository(t, "")
	requests := serveArchive(t, http.StatusOK, archiveBody)
	tools := unpackingTools(t, tarball, "")

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if requests.Load() != 1 {
		t.Errorf("downloaded %d times", requests.Load())
	}

	destination := filepath.Join(root, "third_party", "sparkle")
	for path, want := range map[string]string{
		"Sparkle.framework/Versions/B/Sparkle": "framework binary",
		"bin/sign_update":                      "signer",
		"bin/generate_keys":                    "key generator",
		"LICENSE.sparkle":                      "Sparkle licence",
		"VERSION":                              "2.10.0\n",
	} {
		if got := readText(t, filepath.Join(destination, path)); got != want {
			t.Errorf("%s holds %q, want %q", path, got, want)
		}
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{"LICENSE.sparkle", "Sparkle.framework", "VERSION", "bin"}; !slices.Equal(names, want) {
		t.Errorf("third_party/sparkle holds %v, want only %v", names, want)
	}

	tar := tools.named("tar")
	if len(tar) != 1 || tar[0].args[0] != "xf" || filepath.Base(tar[0].args[1]) != "sparkle.tar.xz" {
		t.Errorf("the archive was not unpacked with tar: %v", tools.lines())
	}
	if !strings.Contains(stdout, "checksum matches") {
		t.Errorf("the checksum was not reported:\n%s", stdout)
	}
}

func TestATarballThatDoesNotMatchThePinIsRefusedAndTheOldCopyKept(t *testing.T) {
	root := newRepository(t, "")
	previous := filepath.Join(root, "third_party", "sparkle", "Sparkle.framework", "Versions", "B", "Sparkle")
	writeFile(t, previous, "the copy that was verified last time")
	serveArchive(t, http.StatusOK, "something else entirely")
	tools := unpackingTools(t, tarball, "")

	code, _, stderr := runMain(t, "--force")
	if code != 1 {
		t.Fatalf("exited %d, want 1", code)
	}
	for _, want := range []string{checksumOf("something else entirely"), checksumOf(archiveBody), "Do not proceed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
	if len(tools.calls) != 0 {
		t.Errorf("an unverified archive was unpacked: %v", tools.lines())
	}
	if got := readText(t, previous); got != "the copy that was verified last time" {
		t.Errorf("the verified copy was replaced by %q", got)
	}
}

func TestAnUnpackedCopyIsKeptUnlessForced(t *testing.T) {
	root := newRepository(t, "")
	writeFile(t, filepath.Join(root, "third_party", "sparkle", "Sparkle.framework", "Versions", "B", "Sparkle"), "old")
	requests := serveArchive(t, http.StatusOK, archiveBody)
	unpackingTools(t, tarball, "")

	code, stdout, _ := runMain(t)
	if code != 0 || !strings.Contains(stdout, "already unpacked") || requests.Load() != 0 {
		t.Fatalf("exited %d after %d downloads:\n%s", code, requests.Load(), stdout)
	}

	code, _, stderr := runMain(t, "--force")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if requests.Load() != 1 {
		t.Errorf("--force downloaded %d times", requests.Load())
	}
	framework := filepath.Join(root, "third_party", "sparkle", "Sparkle.framework", "Versions", "B", "Sparkle")
	if got := readText(t, framework); got != "framework binary" {
		t.Errorf("--force left %q in place", got)
	}
}

func TestTheLicenceIsCopiedWhicheverNameItHas(t *testing.T) {
	root := newRepository(t, "")
	serveArchive(t, http.StatusOK, archiveBody)
	contents := map[string]string{
		"Sparkle.framework/Versions/B/Sparkle": "framework binary",
		"bin/sign_update":                      "signer",
		"LICENSE.md":                           "Sparkle licence in Markdown",
	}
	unpackingTools(t, contents, "")

	if code, _, stderr := runMain(t); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if got := readText(t, filepath.Join(root, "third_party", "sparkle", "LICENSE.sparkle")); got != "Sparkle licence in Markdown" {
		t.Errorf("LICENSE.sparkle holds %q", got)
	}
}

func TestADownloadThatFailsStopsTheBuild(t *testing.T) {
	newRepository(t, "")
	serveArchive(t, http.StatusNotFound, "not here")
	tools := unpackingTools(t, tarball, "")

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "returned 404 Not Found") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if len(tools.calls) != 0 {
		t.Errorf("unpacked a failed download: %v", tools.lines())
	}
}

func TestAnUnreachableServerStopsTheBuild(t *testing.T) {
	newRepository(t, "")
	serveArchive(t, http.StatusOK, archiveBody)
	archiveURL = "http://127.0.0.1:1/Sparkle.tar.xz"
	unpackingTools(t, tarball, "")

	if code, _, stderr := runMain(t); code != 1 || !strings.Contains(stderr, "127.0.0.1:1") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestATarThatFailsStopsTheBuildBeforeTheOldCopyIsRemoved(t *testing.T) {
	root := newRepository(t, "")
	previous := filepath.Join(root, "third_party", "sparkle", "VERSION")
	writeFile(t, previous, "2.9.0\n")
	serveArchive(t, http.StatusOK, archiveBody)
	unpackingTools(t, tarball, "tar")

	code, _, stderr := runMain(t, "--force")
	if code != 1 || !strings.Contains(stderr, "tar xf ") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if got := readText(t, previous); got != "2.9.0\n" {
		t.Errorf("the previous copy was touched: VERSION is %q", got)
	}
}

func TestOutsideARepositoryNothingIsDownloaded(t *testing.T) {
	t.Chdir(t.TempDir())
	requests := serveArchive(t, http.StatusOK, archiveBody)
	unpackingTools(t, tarball, "")

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "Could not find the repository root.") || requests.Load() != 0 {
		t.Errorf("exited %d after %d downloads with %q", code, requests.Load(), stderr)
	}
}
