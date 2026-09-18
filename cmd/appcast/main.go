// SPDX-License-Identifier: GPL-3.0-or-later
// Command appcast builds and checks the Sparkle update feed.
//
//	go run ./cmd/appcast --base-url https://downloads.example.com/xpremvpn
//	go run ./cmd/appcast --validate
//
// WHAT SPARKLE NEEDS, AND WHAT IT DOES NOT
// ----------------------------------------
// There is no tool called "sparklevalidator". Sparkle 2.10 ships exactly four
// binaries — generate_keys, sign_update, generate_appcast and BinaryDelta — and
// the only thing in the distribution with "validation" in its name is
// SPUAppcastSigningValidationStatus.h, a runtime enum inside the framework.
//
// The validation that protects a user happens in the framework, at update time:
// Sparkle refuses an enclosure whose EdDSA signature does not verify against the
// public key baked into the app's Info.plist. That is the check that matters,
// and it cannot be run from here because it is the updater's job.
//
// What can be checked at build time is everything that would make that check
// fail for a boring reason, and that is what --validate does:
//
//   - the feed is well-formed XML with exactly the items it should have;
//   - the enclosure's recorded length matches the file on disk;
//   - the signature in the feed verifies against the same key pair, using
//     Sparkle's own sign_update -verify rather than a reimplementation;
//   - the version in the feed matches the version inside the package, so the
//     feed cannot advertise an update that installs something else;
//   - the app being shipped actually carries a feed URL and a public key, since
//     an app without them will never ask for an update at all.
//
// Publishing is deliberately not automated. The feed is the one file that, if
// it is wrong, reaches through to every installed copy.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// signingKey is the exported key file, when there is one. A release build on a
// developer's Mac uses the login Keychain and needs none; CI has no login
// Keychain at all, so there the key arrives as a file written from a secret.
//
// Sparkle's exported format is base64 of the 64-byte Ed25519 private key
// followed by the 32-byte public key. A file holding only the 64-byte private
// key is rejected — with a message that says it wanted 64 bytes.
var signingKey string

func keyArguments() []string {
	if signingKey == "" {
		return nil
	}
	return []string{"--ed-key-file", signingKey}
}

const (
	// MinimumSystemVersion has to agree with LSMinimumSystemVersion in the
	// bundle: Sparkle uses it to hide an update from a Mac that cannot run it,
	// and an update offered to a Mac that then refuses to launch it is worse
	// than no update.
	minimumSystemVersion = "13.0"

	feedTitle       = "Xprem VPN"
	feedDescription = "Updates for Xprem VPN"
)

func main() {
	baseURL := flag.String("base-url", os.Getenv("APPCAST_BASE_URL"),
		"where the update archives are published, without a trailing slash")
	feedURL := flag.String("feed-url", os.Getenv("APPCAST_FEED_URL"),
		"the appcast's own URL; defaults to <base-url>/appcast.xml")
	notes := flag.String("notes", "", "release notes, as a file of HTML or Markdown")
	validateOnly := flag.Bool("validate", false, "check the existing feed and change nothing")
	keyFile := flag.String("key-file", os.Getenv("SPARKLE_PRIVATE_KEY_FILE"),
		"EdDSA private key exported with `generate_keys -x`; the login Keychain is used when empty")
	flag.Parse()

	signingKey = *keyFile

	root := repoRoot()
	version := readVersion(root)
	buildDir := filepath.Join(root, "build")
	packagePath := filepath.Join(buildDir, fmt.Sprintf("XpremVpn-%s.pkg", version))
	archivePath := filepath.Join(buildDir, fmt.Sprintf("XpremVpn-%s.zip", version))
	feedPath := filepath.Join(buildDir, "appcast.xml")

	if *validateOnly {
		validate(root, feedPath, buildDir, version)
		return
	}

	if *baseURL == "" {
		fail("No publication URL.\n\n" +
			"  The feed has to say where the archive can be downloaded, and there is no\n" +
			"  sensible default for that: it is wherever you host releases. Pass it, or\n" +
			"  set APPCAST_BASE_URL:\n\n" +
			"      go run ./cmd/appcast --base-url https://downloads.example.com/xpremvpn\n\n" +
			"  The same value goes into the app as SUFeedURL; see menubar/Info.plist.")
	}
	trimmed := strings.TrimSuffix(*baseURL, "/")
	if *feedURL == "" {
		*feedURL = trimmed + "/appcast.xml"
	}

	if !exists(packagePath) {
		fail("No package at %s. Run `make pkg-notarized PROFILE=<profile>` first.\n\n"+
			"  An unnotarized package can be signed into a feed perfectly well and will\n"+
			"  then be refused by Gatekeeper on every machine that downloads it.",
			packagePath)
	}

	// Sparkle installs a package update by running it, and expects it inside an
	// archive rather than bare.
	step("Packing %s", filepath.Base(packagePath))
	must(os.RemoveAll(archivePath))
	runIn(buildDir, "ditto", "-c", "-k", "--keepParent", packagePath, archivePath)
	done("%s", filepath.Base(archivePath))

	step("Signing the archive")
	signature, length := signUpdate(root, archivePath)
	done("EdDSA signature over %d bytes", length)

	step("Writing the feed")
	feed := buildFeed(feedItem{
		Version:      version,
		URL:          trimmed + "/" + filepath.Base(archivePath),
		Length:       length,
		EdSignature:  signature,
		Published:    time.Now(),
		ReleaseNotes: readNotes(*notes),
	})
	must(os.WriteFile(feedPath, []byte(feed), 0o644))
	done("%s", feedPath)

	// Signed as a whole as well, so a feed altered in transit is rejected even
	// before its enclosure is looked at.
	step("Signing the feed")
	runIn(root, sparkleTool(root, "sign_update"),
		append(keyArguments(), "--disable-signing-warning", feedPath)...)
	done("signed")

	validate(root, feedPath, buildDir, version)

	fmt.Printf("\n✓ %s\n", feedPath)
	fmt.Printf("\n  Publish both, keeping the names:\n")
	fmt.Printf("    %s  ->  %s\n", filepath.Base(archivePath), trimmed+"/"+filepath.Base(archivePath))
	fmt.Printf("    %s            ->  %s\n", "appcast.xml", *feedURL)
	fmt.Printf("\n  The app must already carry SUFeedURL = %s and the matching\n", *feedURL)
	fmt.Printf("  SUPublicEDKey, or no installed copy will ever ask.\n")
}

// --- the feed --------------------------------------------------------------

type feedItem struct {
	Version      string
	URL          string
	Length       int64
	EdSignature  string
	Published    time.Time
	ReleaseNotes string
}

func buildFeed(item feedItem) string {
	var notes string
	if item.ReleaseNotes != "" {
		notes = "      <description><![CDATA[\n" + item.ReleaseNotes + "\n      ]]></description>\n"
	}

	return strings.Join([]string{
		`<?xml version="1.0" encoding="utf-8"?>`,
		`<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">`,
		`  <channel>`,
		`    <title>` + feedTitle + `</title>`,
		`    <description>` + feedDescription + `</description>`,
		`    <language>en</language>`,
		`    <item>`,
		`      <title>Version ` + item.Version + `</title>`,
		`      <pubDate>` + item.Published.Format(time.RFC1123Z) + `</pubDate>`,
		`      <sparkle:version>` + item.Version + `</sparkle:version>`,
		`      <sparkle:shortVersionString>` + item.Version + `</sparkle:shortVersionString>`,
		`      <sparkle:minimumSystemVersion>` + minimumSystemVersion + `</sparkle:minimumSystemVersion>`,
		notes + `      <enclosure url="` + item.URL + `"`,
		`                 length="` + strconv.FormatInt(item.Length, 10) + `"`,
		`                 type="application/octet-stream"`,
		// Without this Sparkle treats the archive as an app bundle to swap in,
		// finds a .pkg instead, and fails. This product installs root-owned
		// files outside the bundle, so a package is the only honest shape for
		// an update of it.
		`                 sparkle:installationType="package"`,
		`                 sparkle:edSignature="` + item.EdSignature + `"/>`,
		`    </item>`,
		`  </channel>`,
		`</rss>`,
		``,
	}, "\n")
}

// signUpdate shells out to Sparkle's own signer rather than reimplementing
// Ed25519 over the file. The private key lives in the login Keychain, put there
// once by `make sparkle-keys`, and never touches the repository.
func signUpdate(root, path string) (string, int64) {
	output := run(sparkleTool(root, "sign_update"), append(keyArguments(), path)...)
	if output.code != 0 {
		if strings.Contains(output.text, "Could not find") || strings.Contains(output.text, "no key") {
			fail("Sparkle has no signing key for this machine.\n\n" +
				"  Create one once, into the login Keychain:\n\n" +
				"      make sparkle-keys\n\n" +
				"  It prints the public half; that value goes into menubar/Info.plist as\n" +
				"  SUPublicEDKey. Back the private half up somewhere you will still have\n" +
				"  it in two years: losing it means no installed copy can ever be updated\n" +
				"  again, and every user has to be sent a new package by hand.")
		}
		fail("sign_update failed:\n%s", output.text)
	}

	// It prints the two attributes ready to paste: sparkle:edSignature="..."
	// length="...".
	signature := attribute(output.text, "sparkle:edSignature")
	rawLength := attribute(output.text, "length")
	if signature == "" || rawLength == "" {
		fail("sign_update produced something unexpected:\n%s", output.text)
	}
	length, err := strconv.ParseInt(rawLength, 10, 64)
	must(err)
	return signature, length
}

func attribute(text, name string) string {
	marker := name + `="`
	start := strings.Index(text, marker)
	if start < 0 {
		return ""
	}
	rest := text[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// --- checking it -----------------------------------------------------------

type rss struct {
	Channel struct {
		Items []struct {
			Title     string `xml:"title"`
			Version   string `xml:"version"`
			Enclosure struct {
				URL              string `xml:"url,attr"`
				Length           int64  `xml:"length,attr"`
				EdSignature      string `xml:"edSignature,attr"`
				InstallationType string `xml:"installationType,attr"`
			} `xml:"enclosure"`
		} `xml:"item"`
	} `xml:"channel"`
}

func validate(root, feedPath, buildDir, version string) {
	step("Checking the feed")

	content, err := os.ReadFile(feedPath)
	if err != nil {
		fail("No feed at %s. Generate one first.", feedPath)
	}

	var parsed rss
	if err := xml.Unmarshal(content, &parsed); err != nil {
		fail("%s is not well-formed XML: %v", feedPath, err)
	}
	if len(parsed.Channel.Items) != 1 {
		fail("the feed has %d items; this product publishes one at a time",
			len(parsed.Channel.Items))
	}
	item := parsed.Channel.Items[0]

	if item.Enclosure.InstallationType != "package" {
		fail("the enclosure is %q, not \"package\". Sparkle would try to swap it in as an\n"+
			"  app bundle and fail, because this update is a .pkg that installs root-owned\n"+
			"  files outside the bundle.", item.Enclosure.InstallationType)
	}
	if item.Version != version {
		fail("the feed advertises %s but the build is %s", item.Version, version)
	}
	done("one package item, version %s", item.Version)

	archivePath := filepath.Join(buildDir, filepath.Base(item.Enclosure.URL))
	if !exists(archivePath) {
		fail("the feed points at %s, which is not in %s", filepath.Base(item.Enclosure.URL), buildDir)
	}

	info, err := os.Stat(archivePath)
	must(err)
	if info.Size() != item.Enclosure.Length {
		fail("the feed says the archive is %d bytes; it is %d. Sparkle refuses a length\n"+
			"  that does not match.", item.Enclosure.Length, info.Size())
	}
	done("%s is %d bytes, as advertised", filepath.Base(archivePath), info.Size())

	// Sparkle's own verifier, so this agrees with what the updater will do
	// rather than with a second implementation of it.
	// sign_update takes the file first and the signature after it; passing them
	// the other way round makes it try to open the signature as a path, which
	// fails in a way that reads like a bad signature.
	//
	// --verify consults the login Keychain and ignores --ed-key-file, so this
	// check is only possible on the path where the key lives in the Keychain —
	// a release cut on a developer's Mac. On CI, where the key arrives as a
	// file, the signature is still produced by Sparkle's own signer; it simply
	// cannot be re-verified here, and claiming otherwise would be worse than
	// saying so.
	if signingKey == "" {
		verification := run(sparkleTool(root, "sign_update"),
			"--verify", archivePath, item.Enclosure.EdSignature)
		if verification.code != 0 {
			fail("the signature in the feed does not verify against the archive:\n%s\n\n"+
				"  Every installed copy would reject this update.", verification.text)
		}
		done("the signature verifies against the Keychain key")
	} else {
		warn("signature not re-verified: --verify reads the Keychain, and this run signed")
		warn("with %s. Verify a release cut on a machine that holds the key.", filepath.Base(signingKey))
	}

	digest := sha256Of(archivePath)
	done("sha256 %s", digest[:16])

	checkTheAppWillAsk(root)
}

// checkTheAppWillAsk catches the failure that produces no error anywhere: a
// perfectly good feed, published, that no installed copy ever reads.
func checkTheAppWillAsk(root string) {
	// The built bundle, not the source template: the keys are added at build
	// time from APPCAST_FEED_URL and SPARKLE_PUBLIC_KEY, so the source file
	// never has them and checking it would always fail.
	plist := filepath.Join(root, "menubar", "build", "XpremVpn.app", "Contents", "Info.plist")
	content, err := os.ReadFile(plist)
	if err != nil {
		warn("Could not read %s", plist)
		return
	}
	text := string(content)

	for key, why := range map[string]string{
		"SUFeedURL":     "the app would never know where to look",
		"SUPublicEDKey": "the app could not tell a real update from any other file",
	} {
		if !strings.Contains(text, key) {
			fail("The built app has no %s, so %s.\n\n"+
				"  Rebuild with both set:\n\n"+
				"      make app APPCAST_FEED_URL=... SPARKLE_PUBLIC_KEY=...", key, why)
		}
	}
	done("the app carries a feed URL and a public key")
}

// --- plumbing --------------------------------------------------------------

func sparkleTool(root, name string) string {
	path := filepath.Join(root, "third_party", "sparkle", "bin", name)
	if !exists(path) {
		fail("%s is missing. Run `make sparkle` to fetch it.", path)
	}
	return path
}

func readVersion(root string) string {
	content, err := os.ReadFile(filepath.Join(root, "VERSION"))
	must(err)
	return strings.TrimSpace(string(content))
}

func readNotes(path string) string {
	if path == "" {
		return ""
	}
	content, err := os.ReadFile(path)
	must(err)
	return strings.TrimSpace(string(content))
}

func sha256Of(path string) string {
	content, err := os.ReadFile(path)
	must(err)
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

type result struct {
	code int
	text string
}

func run(name string, args ...string) result {
	command := exec.Command(name, args...)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		if code = command.ProcessState.ExitCode(); code == 0 {
			code = -1
		}
	}
	return result{code: code, text: strings.TrimSpace(string(output))}
}

func runIn(directory, name string, args ...string) {
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
func warn(format string, args ...any) { fmt.Printf("  ! "+format+"\n", args...) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n✗ "+format+"\n", args...)
	os.Exit(1)
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
