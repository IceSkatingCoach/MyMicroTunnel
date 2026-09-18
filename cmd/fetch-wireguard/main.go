// SPDX-License-Identifier: GPL-3.0-or-later
// Command fetch-wireguard builds the one third-party binary this product
// ships: wireguard-go.
//
//	go run ./cmd/fetch-wireguard
//
// macOS has no kernel WireGuard, so something has to move the packets in
// userspace. Everything else the tunnel needs — configuring the interface,
// reading its state — this repository does itself over the UAPI socket, so
// wg(8), wg-quick(8), bash 4 and Homebrew are all things the customer no longer
// has to have. See internal/tunnel for why.
//
// The result is written to third_party/ and is not committed: a binary in a
// repository is a binary nobody can audit. This command is what a release build
// runs first, and CI runs it too, so the version below is the version that
// ships.
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
	// Pinned rather than "latest": a build that silently changes what it ships
	// is a build whose output cannot be explained afterwards.
	wireguardGoVersion = "0.0.20230223"
	wireguardGoSHA256  = "50029ca43196cc3d925c0f0b98a9fcfd7f6c28465122da90ebe5262398f3b31c"
	wireguardGoURL     = "https://git.zx2c4.com/wireguard-go/snapshot/wireguard-go-" +
		wireguardGoVersion + ".tar.xz"
)

// The release pins golang.org/x/net from early 2022, which no longer links
// against a current Go: `link: golang.org/x/net/internal/socket: invalid
// reference to syscall.recvmsg`. These are the versions this build upgrades to,
// pinned for the same reason the tarball is.
var dependencyUpgrades = []string{
	"golang.org/x/net@v0.44.0",
	"golang.org/x/sys@v0.36.0",
	"golang.org/x/crypto@v0.42.0",
}

func main() {
	force := flag.Bool("force", false, "rebuild even when the binary is already present")
	flag.Parse()

	root := repoRoot()
	destination := filepath.Join(root, "third_party")
	binary := filepath.Join(destination, "wireguard-go")

	if !*force && isUniversal(binary) {
		fmt.Printf("✓ %s is already built and universal\n", binary)
		return
	}

	must(os.MkdirAll(destination, 0o755))

	work, err := os.MkdirTemp("", "wireguard-go-")
	must(err)
	defer os.RemoveAll(work)

	archive := filepath.Join(work, "wireguard-go.tar.xz")
	step("Downloading wireguard-go %s", wireguardGoVersion)
	download(wireguardGoURL, archive)
	verify(archive, wireguardGoSHA256)
	done("checksum matches")

	source := filepath.Join(work, "source")
	must(os.MkdirAll(source, 0o755))
	step("Unpacking")
	runIn(work, "tar", "xf", archive, "-C", source, "--strip-components=1")
	done("unpacked")

	step("Upgrading the dependencies that no longer link")
	arguments := append([]string{"get"}, dependencyUpgrades...)
	runInEnv(source, []string{"GOFLAGS=-mod=mod"}, "go", arguments...)
	done("%s", strings.Join(dependencyUpgrades, " "))

	// Universal for the same reason the rest of the package is: the customer's
	// Mac is whichever one they own, and a bundle with one slice missing
	// refuses to launch rather than degrading.
	var slices []string
	for _, architecture := range []string{"arm64", "amd64"} {
		step("Building darwin/%s", architecture)
		slice := filepath.Join(work, "wireguard-go."+architecture)
		runInEnv(source,
			[]string{"GOOS=darwin", "GOARCH=" + architecture, "CGO_ENABLED=0"},
			"go", "build", "-trimpath", "-ldflags", "-s -w", "-o", slice, ".")
		slices = append(slices, slice)
		done("built")
	}

	step("Merging")
	runIn(work, "lipo", append([]string{"-create", "-output", binary}, slices...)...)
	must(os.Chmod(binary, 0o755))
	done("%s", binary)

	// Shipped beside the binary because the licence requires it to travel with
	// the code, and because a customer auditing the package should be able to
	// see what is in it without unpacking a tarball.
	step("Copying the licence")
	copyFile(filepath.Join(source, "LICENSE"), filepath.Join(destination, "LICENSE.wireguard-go"))
	must(os.WriteFile(filepath.Join(destination, "VERSION.wireguard-go"),
		[]byte(wireguardGoVersion+"\n"), 0o644))
	done("third_party/LICENSE.wireguard-go")

	fmt.Printf("\n✓ wireguard-go %s, %s\n", wireguardGoVersion, archs(binary))
}

// --- steps -----------------------------------------------------------------

func download(url, destination string) {
	client := &http.Client{Timeout: 5 * time.Minute}
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

// verify is not a formality. This binary runs as root and carries the tunnel's
// private key; a tarball fetched over a compromised path would be a root
// exploit with a signature on it.
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

func isUniversal(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	info := archs(path)
	return strings.Contains(info, "arm64") && strings.Contains(info, "x86_64")
}

func archs(path string) string {
	output, err := exec.Command("lipo", "-archs", path).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// --- plumbing --------------------------------------------------------------

func runIn(directory, name string, args ...string) {
	runInEnv(directory, nil, name, args...)
}

func runInEnv(directory string, environment []string, name string, args ...string) {
	command := exec.Command(name, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		fail("%s %s: %v", name, strings.Join(args, " "), err)
	}
}

func copyFile(source, destination string) {
	content, err := os.ReadFile(source)
	must(err)
	must(os.WriteFile(destination, content, 0o644))
}

func repoRoot() string {
	working, err := os.Getwd()
	must(err)
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

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n✗ "+format+"\n", args...)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
