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

const archiveBody = "pretend this is wireguard-go-0.0.20230223.tar.xz"

func checksumOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

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
	archiveURL = server.URL + "/wireguard-go.tar.xz"
	archiveSHA256 = checksumOf(archiveBody)
	t.Cleanup(func() { archiveURL, archiveSHA256 = savedURL, savedSum })
	t.Setenv("TMPDIR", t.TempDir())
	return &requests
}

// buildTools stands in for tar, go and lipo. lipo -archs answers with archs
// for a file that exists; failing names the first command line to fail.
func buildTools(t *testing.T, archs, failing string) *fakeTools {
	t.Helper()
	return installTools(t, func(name string, args []string) (string, int) {
		line := name + " " + strings.Join(args, " ")
		if failing != "" && strings.HasPrefix(line, failing) {
			return "", 1
		}
		switch {
		case name == "tar":
			writeFile(t, filepath.Join(args[slices.Index(args, "-C")+1], "LICENSE"), "MIT licence")
		case name == "go" && args[0] == "build":
			writeFile(t, args[slices.Index(args, "-o")+1], "slice")
		case name == "lipo" && args[0] == "-create":
			writeFile(t, args[2], "universal binary")
		case name == "lipo" && args[0] == "-archs":
			if _, err := os.Stat(args[1]); err != nil {
				return "can't open input file", 1
			}
			return archs, 0
		}
		return "", 0
	})
}

func TestThePinNamesTheVersionItClaims(t *testing.T) {
	if archiveURL != wireguardGoURL || archiveSHA256 != wireguardGoSHA256 {
		t.Fatalf("the build does not download the pinned release")
	}
	if !strings.HasSuffix(wireguardGoURL, "/wireguard-go-"+wireguardGoVersion+".tar.xz") {
		t.Errorf("%s is not the %s release", wireguardGoURL, wireguardGoVersion)
	}
	if len(wireguardGoSHA256) != 64 {
		t.Errorf("the pinned checksum %q is not a sha256", wireguardGoSHA256)
	}
}

func TestAVerifiedDownloadIsBuiltForBothArchitecturesAndMerged(t *testing.T) {
	root := newRepository(t, "")
	requests := serveArchive(t, http.StatusOK, archiveBody)
	tools := buildTools(t, "x86_64 arm64", "")

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if requests.Load() != 1 {
		t.Errorf("downloaded %d times", requests.Load())
	}

	goCalls := tools.named("go")
	if len(goCalls) != 3 {
		t.Fatalf("go ran %d times: %v", len(goCalls), tools.lines())
	}
	wantGet := append([]string{"get"}, dependencyUpgrades...)
	if !slices.Equal(goCalls[0].args, wantGet) || !slices.Contains(goCalls[0].cmd.Env, "GOFLAGS=-mod=mod") {
		t.Errorf("the dependencies were not upgraded with -mod=mod: %s", goCalls[0].line())
	}
	var slicePaths []string
	for index, architecture := range []string{"arm64", "amd64"} {
		call := goCalls[index+1]
		for _, want := range []string{"GOOS=darwin", "GOARCH=" + architecture, "CGO_ENABLED=0"} {
			if !slices.Contains(call.cmd.Env, want) {
				t.Errorf("the %s build is missing %s", architecture, want)
			}
		}
		if !slices.Contains(call.args, "-trimpath") || call.args[len(call.args)-1] != "." {
			t.Errorf("the %s build is not reproducible: %s", architecture, call.line())
		}
		output := call.args[slices.Index(call.args, "-o")+1]
		if filepath.Base(output) != "wireguard-go."+architecture {
			t.Errorf("the %s slice is written to %s", architecture, output)
		}
		slicePaths = append(slicePaths, output)
	}

	binary := filepath.Join(root, "third_party", "wireguard-go")
	var merge []string
	for _, call := range tools.named("lipo") {
		if call.args[0] == "-create" {
			merge = call.args
		}
	}
	if want := append([]string{"-create", "-output", binary}, slicePaths...); !slices.Equal(merge, want) {
		t.Errorf("lipo merged %q, want %q", merge, want)
	}

	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("wireguard-go is %v, want executable", info.Mode().Perm())
	}
	if got := readText(t, filepath.Join(root, "third_party", "LICENSE.wireguard-go")); got != "MIT licence" {
		t.Errorf("the licence holds %q", got)
	}
	if got := readText(t, filepath.Join(root, "third_party", "VERSION.wireguard-go")); got != wireguardGoVersion+"\n" {
		t.Errorf("VERSION.wireguard-go holds %q", got)
	}
	if !strings.Contains(stdout, "wireguard-go "+wireguardGoVersion+", x86_64 arm64") {
		t.Errorf("the summary does not name the architectures:\n%s", stdout)
	}
}

func TestATarballThatDoesNotMatchThePinIsNeverBuilt(t *testing.T) {
	newRepository(t, "")
	serveArchive(t, http.StatusOK, "a tarball someone swapped in")
	tools := buildTools(t, "x86_64 arm64", "")

	code, _, stderr := runMain(t)
	if code != 1 {
		t.Fatalf("exited %d, want 1", code)
	}
	for _, want := range []string{checksumOf("a tarball someone swapped in"), checksumOf(archiveBody), "Do not proceed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr)
		}
	}
	for _, call := range tools.calls {
		if call.name != "lipo" || call.args[0] != "-archs" {
			t.Errorf("ran %s on an unverified archive", call.line())
		}
	}
}

func TestAUniversalBinaryIsKeptUnlessForced(t *testing.T) {
	root := newRepository(t, "")
	writeFile(t, filepath.Join(root, "third_party", "wireguard-go"), "already built")
	requests := serveArchive(t, http.StatusOK, archiveBody)
	buildTools(t, "x86_64 arm64", "")

	code, stdout, _ := runMain(t)
	if code != 0 || !strings.Contains(stdout, "already built and universal") || requests.Load() != 0 {
		t.Fatalf("exited %d after %d downloads:\n%s", code, requests.Load(), stdout)
	}

	if code, _, stderr := runMain(t, "--force"); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if requests.Load() != 1 {
		t.Errorf("--force downloaded %d times", requests.Load())
	}
}

func TestASingleArchitectureBinaryIsRebuilt(t *testing.T) {
	root := newRepository(t, "")
	writeFile(t, filepath.Join(root, "third_party", "wireguard-go"), "arm64 only")
	requests := serveArchive(t, http.StatusOK, archiveBody)
	buildTools(t, "arm64", "")

	if code, _, stderr := runMain(t); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if requests.Load() != 1 {
		t.Errorf("a one-slice binary was kept; downloaded %d times", requests.Load())
	}
}

func TestEachStepThatFailsStopsTheBuild(t *testing.T) {
	for _, test := range []struct {
		failing string
		want    string
	}{
		{"tar ", "tar xf "},
		{"go get", "go get golang.org/x/net@v0.44.0"},
		{"go build", "go build -trimpath"},
		{"lipo -create", "lipo -create -output"},
	} {
		t.Run(test.failing, func(t *testing.T) {
			root := newRepository(t, "")
			serveArchive(t, http.StatusOK, archiveBody)
			buildTools(t, "x86_64 arm64", test.failing)

			code, _, stderr := runMain(t)
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q, want %q", code, stderr, test.want)
			}
			if _, err := os.Stat(filepath.Join(root, "third_party", "VERSION.wireguard-go")); err == nil {
				t.Errorf("a failed build recorded a version")
			}
		})
	}
}

func TestADownloadThatFailsStopsTheBuild(t *testing.T) {
	newRepository(t, "")
	serveArchive(t, http.StatusBadGateway, "")
	buildTools(t, "", "")

	if code, _, stderr := runMain(t); code != 1 || !strings.Contains(stderr, "returned 502 Bad Gateway") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestOutsideARepositoryNothingIsDownloaded(t *testing.T) {
	t.Chdir(t.TempDir())
	requests := serveArchive(t, http.StatusOK, archiveBody)
	buildTools(t, "", "")

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "Could not find the repository root.") || requests.Load() != 0 {
		t.Errorf("exited %d after %d downloads with %q", code, requests.Load(), stderr)
	}
}
