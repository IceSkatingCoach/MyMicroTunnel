// SPDX-License-Identifier: GPL-3.0-or-later
// Command fetch-sparkle downloads the Sparkle framework used for updates.
//
//	go run ./cmd/fetch-sparkle
//
// Sparkle is fetched rather than vendored for the same reason wireguard-go is:
// a framework committed to a repository is a binary nobody can audit. The
// release is pinned and its checksum verified, so what ships is what this file
// says ships.
//
// The result lands in third_party/sparkle/ and is not committed:
//
//	Sparkle.framework   embedded in XpremVpn.app/Contents/Frameworks
//	bin/generate_keys   creates the EdDSA keypair, once, into the Keychain
//	bin/sign_update     signs a package and the appcast that points at it
//	bin/generate_appcast  builds a whole feed from a directory of updates
//
// There is no tool called "sparklevalidator". The validation that matters
// happens in the framework at update time — it refuses an update whose EdDSA
// signature does not match the public key baked into the app — and the build's
// own check of that is in cmd/appcast.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	sparkleVersion = "2.10.0"
	sparkleSHA256  = "c2bf58aa8387266ac179357b1415d6f2635f044da8be41042af32425dae6da0c"
	sparkleURL     = "https://github.com/sparkle-project/Sparkle/releases/download/" +
		sparkleVersion + "/Sparkle-" + sparkleVersion + ".tar.xz"
)

func main() {
	force := flag.Bool("force", false, "re-download even when it is already present")
	flag.Parse()

	root := repoRoot()
	destination := filepath.Join(root, "third_party", "sparkle")
	framework := filepath.Join(destination, "Sparkle.framework")

	if !*force && exists(filepath.Join(framework, "Versions", "B", "Sparkle")) {
		fmt.Printf("✓ Sparkle %s is already unpacked at %s\n", sparkleVersion, destination)
		return
	}

	work, err := os.MkdirTemp("", "sparkle-")
	must(err)
	defer os.RemoveAll(work)

	archive := filepath.Join(work, "sparkle.tar.xz")
	step("Downloading Sparkle %s", sparkleVersion)
	download(sparkleURL, archive)
	verify(archive, sparkleSHA256)
	done("checksum matches")

	step("Unpacking")
	unpacked := filepath.Join(work, "sparkle")
	must(os.MkdirAll(unpacked, 0o755))
	run(work, "tar", "xf", archive, "-C", unpacked)

	must(os.RemoveAll(destination))
	must(os.MkdirAll(destination, 0o755))

	// Only the two things a build needs. The tarball also carries a test app,
	// dSYM bundles and the old DSA scripts, none of which should end up near a
	// release.
	run(work, "cp", "-R", filepath.Join(unpacked, "Sparkle.framework"), framework)
	run(work, "cp", "-R", filepath.Join(unpacked, "bin"), filepath.Join(destination, "bin"))
	done("Sparkle.framework and the signing tools")

	must(os.WriteFile(filepath.Join(destination, "VERSION"), []byte(sparkleVersion+"\n"), 0o644))

	// Apple's licence requires the notice to travel with the binary.
	for _, name := range []string{"LICENSE", "LICENSE.md"} {
		source := filepath.Join(unpacked, name)
		if exists(source) {
			run(work, "cp", source, filepath.Join(destination, "LICENSE.sparkle"))
			break
		}
	}

	fmt.Printf("\n✓ Sparkle %s at %s\n", sparkleVersion, destination)
}

func download(url, destination string) {
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Get(url)
	must(err)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		fail("%s returned %s", url, response.Status)
	}

	file, err := os.Create(destination)
	must(err)
	defer file.Close()

	_, err = io.Copy(file, response.Body)
	must(err)
}

// verify is not a formality. This framework ends up signed with the same
// Developer ID as the rest of the product and runs an installer as root; a
// tarball fetched over a compromised path would inherit all of that.
func verify(path, expected string) {
	file, err := os.Open(path)
	must(err)
	defer file.Close()

	digest := sha256.New()
	_, err = io.Copy(digest, file)
	must(err)

	actual := hex.EncodeToString(digest.Sum(nil))
	if actual != expected {
		fail("%s has checksum\n    %s\n  but this build expects\n    %s\n\n"+
			"  Either the pin is stale or the download is not what it says it is. "+
			"Do not proceed until you know which.", filepath.Base(path), actual, expected)
	}
}

func run(directory, name string, args ...string) {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		fail("%s %s: %v", name, strings.Join(args, " "), err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func repoRoot() string {
	working, err := os.Getwd()
	must(err)
	for attempt := 0; attempt < 4; attempt++ {
		if exists(filepath.Join(working, "go.mod")) {
			return working
		}
		working = filepath.Dir(working)
	}
	fail("Could not find the repository root.")
	return ""
}

func step(format string, args ...any) { fmt.Printf("\n▸ "+format+"\n", args...) }
func done(format string, args ...any) { fmt.Printf("  ✓ "+format+"\n", args...) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n✗ "+format+"\n", args...)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
