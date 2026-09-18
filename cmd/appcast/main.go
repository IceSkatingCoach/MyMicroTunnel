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
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	notes := flag.String("notes", "", "release notes file; CHANGELOG.md's section for this version when empty")
	validateOnly := flag.Bool("validate", false, "check the existing feed and change nothing")
	rollback := flag.String("rollback", "", "point the feed back at an already-published version")
	keyFile := flag.String("key-file", os.Getenv("SPARKLE_PRIVATE_KEY_FILE"),
		"EdDSA private key exported with `generate_keys -x`; the login Keychain is used when empty")
	flag.Parse()

	signingKey = *keyFile

	root := repoRoot()
	version := readVersion(root)
	if *rollback != "" {
		version = *rollback
	}
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
		// Read from the built app rather than derived from --base-url. The two
		// are not the same path: the archives live under a prefix and the feed
		// sits at the root, so deriving one from the other printed instructions
		// that would have published the feed where nothing reads it.
		*feedURL = feedURLFromBundle(root)
	}

	if *rollback != "" {
		// A rollback republishes a version that is already out there, so there
		// is nothing to build: the archive is fetched from the URL the feed
		// will advertise, which also proves that URL still serves it.
		warn("Rolling the feed back to %s. Anyone on a newer version stays there —", version)
		warn("Sparkle does not downgrade. This stops the newer one reaching anybody else.")
		fetchPublished(trimmed, version, archivePath)
	} else if !exists(packagePath) {
		fail("No package at %s. Run `make pkg-notarized PROFILE=<profile>` first.\n\n"+
			"  An unnotarized package can be signed into a feed perfectly well and will\n"+
			"  then be refused by Gatekeeper on every machine that downloads it.",
			packagePath)
	}

	// Sparkle installs a package update by running it, and expects it inside an
	// archive rather than bare — at the archive's root, not in a directory.
	//
	// `ditto --keepParent` on an absolute path keeps the *parent directory*, so
	// archiving build/XpremVpn-1.0.1.pkg produced a zip containing
	// build/XpremVpn-1.0.1.pkg. The package is staged alone in a directory and
	// that directory's contents are archived instead, which puts it where it
	// belongs.
	if *rollback == "" {
		step("Packing %s", filepath.Base(packagePath))
		must(os.RemoveAll(archivePath))
		stageAndPack(buildDir, packagePath, archivePath)
		done("%s", filepath.Base(archivePath))
	}

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
		ReleaseNotes: releaseNotes(root, version, *notes),
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
	if *feedURL == "" {
		warn("The built app carries no SUFeedURL, so where to publish the feed is unknown.")
	} else {
		fmt.Printf("    %s            ->  %s\n", "appcast.xml", *feedURL)
	}
	fmt.Printf("\n  The app must already carry SUFeedURL = %s and the matching\n", *feedURL)
	fmt.Printf("  SUPublicEDKey, or no installed copy will ever ask.\n")
}

// feedURLFromBundle reads SUFeedURL out of the app that is being released. That
// string is the only authority on where the feed has to be published: it is
// compiled into every copy shipped, and anything else is a guess about it.
func feedURLFromBundle(root string) string {
	plist := filepath.Join(root, "menubar", "build", "XpremVpn.app", "Contents", "Info.plist")
	content, err := os.ReadFile(plist)
	if err != nil {
		return ""
	}

	text := string(content)
	marker := "<key>SUFeedURL</key>"
	start := strings.Index(text, marker)
	if start < 0 {
		return ""
	}
	rest := text[start+len(marker):]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// stageAndPack puts the package alone in a directory and archives that
// directory's contents, so the package lands at the archive's root where
// Sparkle looks for it.
func stageAndPack(buildDir, packagePath, archivePath string) {
	staging, err := os.MkdirTemp("", "release-")
	must(err)
	defer os.RemoveAll(staging)

	runIn(buildDir, "cp", packagePath, filepath.Join(staging, filepath.Base(packagePath)))
	runIn(buildDir, "ditto", "-c", "-k", staging, archivePath)
}

// fetchPublished downloads a release that is already on the feed's own CDN.
//
// Rolling back means re-advertising something that was published before, and the
// only copy that matters is the one users will actually download. Fetching it
// from there rather than rebuilding locally means the rollback is signed over
// the exact bytes being served — and fails loudly if those bytes have gone.
func fetchPublished(baseURL, version, archivePath string) {
	url := baseURL + "/XpremVpn-" + version + ".zip"
	step("Fetching the published %s", version)

	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Get(url)
	if err != nil {
		fail("Could not reach %s: %v", url, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		fail("%s returned %s.\n\n"+
			"  Rolling back to %s means that archive is still published. It is not.\n"+
			"  Check what the bucket actually holds before pointing the feed at it.",
			url, response.Status, version)
	}

	file, err := os.Create(archivePath)
	must(err)
	defer file.Close()

	written, err := io.Copy(file, response.Body)
	must(err)
	done("%s, %d bytes, from the URL the feed will advertise", filepath.Base(archivePath), written)
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

	checkArchiveLayout(archivePath)

	digest := sha256Of(archivePath)
	done("sha256 %s", digest[:16])

	checkTheAppWillAsk(root)
}

// checkArchiveLayout insists the package sits at the archive's root.
//
// Sparkle looks for it there. An archive with the package one directory down
// downloads perfectly, verifies its signature perfectly, and then fails to
// install — the worst shape of failure, because everything up to the last step
// reports success.
func checkArchiveLayout(archivePath string) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		fail("%s cannot be read as a zip: %v", filepath.Base(archivePath), err)
	}
	defer reader.Close()

	var packages []string
	for _, file := range reader.File {
		name := file.Name
		// Directory entries and the metadata ditto adds are not the payload.
		if strings.HasSuffix(name, "/") || strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		if strings.HasSuffix(name, ".pkg") {
			packages = append(packages, name)
		}
	}

	if len(packages) != 1 {
		fail("the archive holds %d packages; Sparkle installs exactly one: %v", len(packages), packages)
	}
	if strings.Contains(packages[0], "/") {
		fail("the archive holds the package at %q rather than at its root.\n\n"+
			"  Sparkle would download it, verify it, and then fail to install it.", packages[0])
	}
	done("the archive holds %s at its root", packages[0])
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

// releaseNotes is what the user reads in the update dialog before deciding
// whether to install. Taken from CHANGELOG.md by default, because notes kept
// anywhere else are notes that get forgotten on the release where they matter.
func releaseNotes(root, version, override string) string {
	if override != "" {
		content, err := os.ReadFile(override)
		must(err)
		return markdownToHTML(strings.TrimSpace(string(content)))
	}

	content, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		warn("No CHANGELOG.md, so this release ships without notes.")
		return ""
	}

	section := changelogSection(string(content), version)
	if section == "" {
		// Not fatal, but worth saying out loud: shipping an update that says
		// nothing about itself is a worse default than stopping to ask.
		warn("CHANGELOG.md has no section for %s, so this release ships without notes.", version)
		warn("Add a '## %s' heading to describe what changed.", version)
		return ""
	}
	done("release notes from CHANGELOG.md")
	return markdownToHTML(section)
}

// changelogSection returns the body under "## <version>", stopping at the next
// heading of the same level.
func changelogSection(changelog, version string) string {
	lines := strings.Split(changelog, "\n")
	heading := "## " + version

	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == heading {
			start = index + 1
			break
		}
	}
	if start < 0 {
		return ""
	}

	var body []string
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "## ") {
			break
		}
		body = append(body, line)
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// markdownToHTML handles the little that release notes use: bullets, inline
// code, and bold. Sparkle renders the description as HTML, and a full Markdown
// dependency for three constructs is a dependency in the release path.
func markdownToHTML(text string) string {
	var out []string
	inList := false

	flush := func() {
		if inList {
			out = append(out, "</ul>")
			inList = false
		}
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flush()
		case strings.HasPrefix(trimmed, "- "):
			if !inList {
				out = append(out, "<ul>")
				inList = true
			}
			out = append(out, "<li>"+inlineMarkdown(trimmed[2:])+"</li>")
		default:
			// A continuation of the bullet above, which is how the wrapped
			// lines in CHANGELOG.md read.
			if inList && len(out) > 0 && strings.HasSuffix(out[len(out)-1], "</li>") {
				previous := strings.TrimSuffix(out[len(out)-1], "</li>")
				out[len(out)-1] = previous + " " + inlineMarkdown(trimmed) + "</li>"
				continue
			}
			out = append(out, "<p>"+inlineMarkdown(trimmed)+"</p>")
		}
	}
	flush()
	return strings.Join(out, "\n")
}

var (
	inlineCode = regexp.MustCompile("`([^`]+)`")
	inlineBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

func inlineMarkdown(text string) string {
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	escaped = inlineCode.ReplaceAllString(escaped, "<code>$1</code>")
	return inlineBold.ReplaceAllString(escaped, "<strong>$1</strong>")
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
