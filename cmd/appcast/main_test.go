// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"archive/zip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	signature = "c2lnbmVkIGJ5IHRoZSB0ZXN0"

	builtInfoPlist = `<plist><dict>
	<key>SUFeedURL</key>
	<string> https://downloads.example.com/appcast.xml </string>
	<key>SUPublicEDKey</key>
	<string>cHVibGlj</string>
</dict></plist>`
)

// sparkle stands in for sign_update, cp and ditto. verifyFails makes
// `sign_update --verify` reject the signature; signOutput, when set, replaces
// what signing an archive prints.
type sparkle struct {
	verifyFails bool
	signOutput  *string
	signCode    int
}

func (s sparkle) install(t *testing.T) *fakeTools {
	t.Helper()
	return installTools(t, func(name string, args []string) (string, int) {
		switch filepath.Base(name) {
		case "cp":
			writeFile(t, args[1], readText(t, args[0]))
		case "ditto":
			packDirectory(t, args[len(args)-2], args[len(args)-1])
		case "sign_update":
			switch {
			case slices.Contains(args, "--verify"):
				if s.verifyFails || args[len(args)-1] != signature {
					return "ERROR! Signature verification failed", 1
				}
			case slices.Contains(args, "--disable-signing-warning"):
			case s.signOutput != nil:
				return *s.signOutput, s.signCode
			default:
				info, err := os.Stat(args[len(args)-1])
				if err != nil {
					return err.Error(), 1
				}
				return fmt.Sprintf(`sparkle:edSignature="%s" length="%d"`, signature, info.Size()), 0
			}
		}
		return "", 0
	})
}

// packDirectory zips a directory's files at the archive's root, as ditto -c -k
// does with a directory's contents.
func packDirectory(t *testing.T, directory, archive string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, entry := range entries {
		files[entry.Name()] = readText(t, filepath.Join(directory, entry.Name()))
	}
	writeZip(t, archive, files)
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := zip.NewWriter(file)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		entry.Write([]byte(content))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// newBuild lays out a checkout after `make pkg-notarized`, with Sparkle's
// tools fetched and the app built carrying infoPlist ("" for no app).
func newBuild(t *testing.T, infoPlist string) string {
	t.Helper()
	for _, name := range []string{"APPCAST_BASE_URL", "APPCAST_FEED_URL", "SPARKLE_PRIVATE_KEY_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("TMPDIR", t.TempDir())
	saved := signingKey
	t.Cleanup(func() { signingKey = saved })

	root := newRepository(t, "1.0.3")
	writeFile(t, filepath.Join(root, "build", "MyMicroTunnel-1.0.3.pkg"), "the notarized package")
	writeFile(t, filepath.Join(root, "third_party", "sparkle", "bin", "sign_update"), "")
	writeFile(t, filepath.Join(root, "CHANGELOG.md"), sampleChangelog)
	if infoPlist != "" {
		writeFile(t, filepath.Join(root, "menubar", "build", "MyMicroTunnel.app", "Contents", "Info.plist"), infoPlist)
	}
	return root
}

func TestAReleaseIsPackedSignedAndDescribedByTheFeed(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	tools := sparkle{}.install(t)

	code, stdout, stderr := runMain(t, "--base-url", "https://downloads.example.com/releases/")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}

	build := filepath.Join(root, "build")
	archive := filepath.Join(build, "MyMicroTunnel-1.0.3.zip")
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) != 1 || reader.File[0].Name != "MyMicroTunnel-1.0.3.pkg" {
		t.Errorf("the archive does not hold the package alone at its root: %v", reader.File)
	}

	ditto := tools.named("ditto")
	if len(ditto) != 1 || !slices.Equal(ditto[0].args[:4], []string{"-c", "-k", "--norsrc", "--noextattr"}) ||
		ditto[0].args[5] != archive {
		t.Errorf("ditto was not told to leave extended attributes out: %v", tools.lines())
	}

	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	feed := readText(t, filepath.Join(build, "appcast.xml"))
	for _, want := range []string{
		`<sparkle:version>1.0.3</sparkle:version>`,
		`<sparkle:minimumSystemVersion>13.0</sparkle:minimumSystemVersion>`,
		`url="https://downloads.example.com/releases/MyMicroTunnel-1.0.3.zip"`,
		fmt.Sprintf(`length="%d"`, info.Size()),
		`sparkle:installationType="package"`,
		`sparkle:edSignature="` + signature + `"`,
		`<code>mymicrotunnel diagnose</code>`,
	} {
		if !strings.Contains(feed, want) {
			t.Errorf("the feed is missing %s:\n%s", want, feed)
		}
	}
	if strings.Contains(feed, "one directory down") {
		t.Errorf("the feed carries another release's notes:\n%s", feed)
	}

	sparkleTool := filepath.Join(root, "third_party", "sparkle", "bin", "sign_update")
	var signs []string
	for _, call := range tools.named(sparkleTool) {
		signs = append(signs, strings.Join(call.args, " "))
	}
	want := []string{
		archive,
		"--disable-signing-warning " + filepath.Join(build, "appcast.xml"),
		"--verify " + archive + " " + signature,
	}
	if !slices.Equal(signs, want) {
		t.Errorf("sign_update ran\n%s\nwant\n%s", strings.Join(signs, "\n"), strings.Join(want, "\n"))
	}

	for _, want := range []string{
		"the signature verifies against the Keychain key",
		"the app carries a feed URL and a public key",
		"appcast.xml            ->  https://downloads.example.com/appcast.xml",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not say %q:\n%s", want, stdout)
		}
	}
}

func TestAKeyFileIsPassedToTheSignerAndTheKeychainCheckIsSkipped(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	key := filepath.Join(t.TempDir(), "sparkle-private.key")
	tools := sparkle{}.install(t)

	code, stdout, stderr := runMain(t, "--base-url", "https://downloads.example.com/releases", "--key-file", key)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	for _, call := range tools.named(filepath.Join(root, "third_party", "sparkle", "bin", "sign_update")) {
		if slices.Contains(call.args, "--verify") {
			t.Errorf("--verify ran although it ignores the key file: %s", call.line())
		} else if !slices.Equal(call.args[:2], []string{"--ed-key-file", key}) {
			t.Errorf("signed without the key file: %s", call.line())
		}
	}
	if !strings.Contains(stdout, "signature not re-verified") || !strings.Contains(stdout, "sparkle-private.key") {
		t.Errorf("the skipped check was not admitted:\n%s", stdout)
	}
}

func TestNotesFromAFileReplaceTheChangelog(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	notes := filepath.Join(t.TempDir(), "notes.md")
	writeFile(t, notes, "- Fixed **one** thing <quickly>.\n")
	sparkle{}.install(t)

	if code, _, stderr := runMain(t, "--base-url", "https://d.example.com", "--notes", notes); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	feed := readText(t, filepath.Join(root, "build", "appcast.xml"))
	if !strings.Contains(feed, "<li>Fixed <strong>one</strong> thing &lt;quickly&gt;.</li>") {
		t.Errorf("the notes file was not used:\n%s", feed)
	}
	if strings.Contains(feed, "remove MyMicroTunnel") {
		t.Errorf("the changelog was used despite --notes:\n%s", feed)
	}
}

func TestAReleaseWithoutNotesSaysSo(t *testing.T) {
	for _, test := range []struct {
		name      string
		changelog string
		want      string
	}{
		{"no changelog", "", "No CHANGELOG.md"},
		{"no section", "# Changelog\n\n## 0.9.0\n\n- Old.\n", "Add a '## 1.0.3' heading"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newBuild(t, builtInfoPlist)
			changelog := filepath.Join(root, "CHANGELOG.md")
			if test.changelog == "" {
				os.Remove(changelog)
			} else {
				writeFile(t, changelog, test.changelog)
			}
			sparkle{}.install(t)

			code, stdout, stderr := runMain(t, "--base-url", "https://d.example.com")
			if code != 0 {
				t.Fatalf("exited %d:\n%s", code, stderr)
			}
			if !strings.Contains(stdout, test.want) {
				t.Errorf("the output does not say %q:\n%s", test.want, stdout)
			}
			if feed := readText(t, filepath.Join(root, "build", "appcast.xml")); strings.Contains(feed, "<description><![CDATA[") {
				t.Errorf("the feed carries notes it has none for:\n%s", feed)
			}
		})
	}
}

func TestWithoutABuiltAppTheFeedURLIsReportedUnknown(t *testing.T) {
	newBuild(t, "")
	sparkle{}.install(t)

	code, stdout, stderr := runMain(t, "--base-url", "https://d.example.com")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	for _, want := range []string{"Could not read", "carries no SUFeedURL"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not say %q:\n%s", want, stdout)
		}
	}
}

func TestAFeedURLFlagIsUsedAsGiven(t *testing.T) {
	newBuild(t, builtInfoPlist)
	sparkle{}.install(t)

	code, stdout, stderr := runMain(t, "--base-url", "https://d.example.com", "--feed-url", "https://feed.example.com/a.xml")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "->  https://feed.example.com/a.xml") {
		t.Errorf("the given feed URL is not where it says to publish:\n%s", stdout)
	}
}

func TestWithoutAPublicationURLNothingIsBuilt(t *testing.T) {
	newBuild(t, builtInfoPlist)
	tools := sparkle{}.install(t)

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "No publication URL") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if len(tools.calls) != 0 {
		t.Errorf("ran tools: %v", tools.lines())
	}
}

func TestWithoutANotarizedPackageNothingIsSigned(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	os.Remove(filepath.Join(root, "build", "MyMicroTunnel-1.0.3.pkg"))
	tools := sparkle{}.install(t)

	code, _, stderr := runMain(t, "--base-url", "https://d.example.com")
	if code != 1 || !strings.Contains(stderr, "make pkg-notarized") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if len(tools.calls) != 0 {
		t.Errorf("ran tools: %v", tools.lines())
	}
}

func TestARollbackSignsTheBytesTheCDNServes(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	served := filepath.Join(t.TempDir(), "served.zip")
	writeZip(t, served, map[string]string{"MyMicroTunnel-1.0.2.pkg": "the package that went out"})
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		http.ServeFile(w, r, served)
	}))
	defer server.Close()
	tools := sparkle{}.install(t)

	code, stdout, stderr := runMain(t, "--base-url", server.URL+"/releases", "--rollback", "1.0.2")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if !slices.Equal(requested, []string{"/releases/MyMicroTunnel-1.0.2.zip"}) {
		t.Errorf("fetched %v", requested)
	}
	if got, want := readText(t, filepath.Join(root, "build", "MyMicroTunnel-1.0.2.zip")), readText(t, served); got != want {
		t.Errorf("the archive signed is not the one served")
	}
	if len(tools.named("ditto")) != 0 || len(tools.named("cp")) != 0 {
		t.Errorf("a rollback repacked: %v", tools.lines())
	}
	feed := readText(t, filepath.Join(root, "build", "appcast.xml"))
	if !strings.Contains(feed, "<sparkle:version>1.0.2</sparkle:version>") ||
		!strings.Contains(feed, `url="`+server.URL+`/releases/MyMicroTunnel-1.0.2.zip"`) {
		t.Errorf("the feed does not point back at 1.0.2:\n%s", feed)
	}
	if !strings.Contains(stdout, "Sparkle does not downgrade") {
		t.Errorf("the rollback's limits were not stated:\n%s", stdout)
	}
}

func TestARollbackToAVersionThatIsGoneIsRefused(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	for _, test := range []struct {
		name, baseURL, want string
	}{
		{"not published", server.URL, "404 Not Found"},
		{"unreachable", "http://127.0.0.1:1", "Could not reach"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newBuild(t, builtInfoPlist)
			sparkle{}.install(t)

			code, _, stderr := runMain(t, "--base-url", test.baseURL, "--rollback", "1.0.2")
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(root, "build", "appcast.xml")); err == nil {
				t.Errorf("a feed was written for a release that is not there")
			}
		})
	}
}

func TestSigningFailuresSayWhatToDo(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		code   int
		want   string
	}{
		{"no key", "ERROR! Could not find signing key in the keychain", 1, "make sparkle-keys"},
		{"another error", "ERROR! disk full", 1, "sign_update failed:\nERROR! disk full"},
		{"unexpected output", "signed it, trust me", 0, "sign_update produced something unexpected"},
		{"a length that is not a number", `sparkle:edSignature="x" length="many"`, 0, "invalid syntax"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newBuild(t, builtInfoPlist)
			sparkle{signOutput: &test.output, signCode: test.code}.install(t)

			code, _, stderr := runMain(t, "--base-url", "https://d.example.com")
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q, want %q", code, stderr, test.want)
			}
			if _, err := os.Stat(filepath.Join(root, "build", "appcast.xml")); err == nil {
				t.Errorf("a feed was written without a signature")
			}
		})
	}
}

func TestWithoutSparkleToolsNothingIsSigned(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	os.Remove(filepath.Join(root, "third_party", "sparkle", "bin", "sign_update"))
	sparkle{}.install(t)

	if code, _, stderr := runMain(t, "--base-url", "https://d.example.com"); code != 1 || !strings.Contains(stderr, "make sparkle") {
		t.Errorf("exited %d with %q", code, stderr)
	}
}

func TestAPackOrCopyThatFailsStopsTheRelease(t *testing.T) {
	for _, tool := range []string{"cp", "ditto"} {
		t.Run(tool, func(t *testing.T) {
			newBuild(t, builtInfoPlist)
			installTools(t, func(name string, args []string) (string, int) {
				if name == tool {
					return "", 1
				}
				return "", 0
			})
			if code, _, stderr := runMain(t, "--base-url", "https://d.example.com"); code != 1 || !strings.HasPrefix(strings.TrimSpace(stderr), "✗ "+tool+" ") {
				t.Errorf("exited %d with %q", code, stderr)
			}
		})
	}
}

// validFeed is what a correct release of 1.0.3 leaves in build/, written
// directly so each check can be broken on its own.
func validFeed(t *testing.T, root string) (feed, archive string) {
	t.Helper()
	archive = filepath.Join(root, "build", "MyMicroTunnel-1.0.3.zip")
	writeZip(t, archive, map[string]string{"MyMicroTunnel-1.0.3.pkg": "package"})
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	feed = buildFeed(feedItem{
		Version:     "1.0.3",
		URL:         "https://d.example.com/MyMicroTunnel-1.0.3.zip",
		Length:      info.Size(),
		EdSignature: signature,
	})
	return feed, archive
}

func TestValidateAcceptsACorrectReleaseAndChangesNothing(t *testing.T) {
	root := newBuild(t, builtInfoPlist)
	feed, _ := validFeed(t, root)
	feedPath := filepath.Join(root, "build", "appcast.xml")
	writeFile(t, feedPath, feed)
	tools := sparkle{}.install(t)

	code, stdout, stderr := runMain(t, "--validate")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if readText(t, feedPath) != feed {
		t.Errorf("--validate changed the feed")
	}
	if len(tools.calls) != 1 || tools.calls[0].args[0] != "--verify" {
		t.Errorf("--validate ran more than the verifier: %v", tools.lines())
	}
	for _, want := range []string{"one package item, version 1.0.3", "as advertised", "at its root", "sha256 "} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not say %q:\n%s", want, stdout)
		}
	}
}

func TestValidateRefusesEachWayAReleaseCanBeWrong(t *testing.T) {
	for _, test := range []struct {
		name      string
		infoPlist string
		breakIt   func(t *testing.T, feed, archive string) string
		verify    bool
		want      string
	}{
		{
			name:    "no feed",
			breakIt: func(t *testing.T, feed, archive string) string { return "" },
			want:    "No feed at",
		},
		{
			name:    "not XML",
			breakIt: func(t *testing.T, feed, archive string) string { return "<rss><channel>" },
			want:    "is not well-formed XML",
		},
		{
			name: "two items",
			breakIt: func(t *testing.T, feed, archive string) string {
				start, end := strings.Index(feed, "    <item>"), strings.Index(feed, "  </channel>")
				return feed[:end] + feed[start:end] + feed[end:]
			},
			want: "the feed has 2 items",
		},
		{
			name: "an app-bundle enclosure",
			breakIt: func(t *testing.T, feed, archive string) string {
				return strings.Replace(feed, `sparkle:installationType="package"`, "", 1)
			},
			want: `the enclosure is "", not "package"`,
		},
		{
			name: "another version",
			breakIt: func(t *testing.T, feed, archive string) string {
				return strings.Replace(feed, "<sparkle:version>1.0.3<", "<sparkle:version>1.0.2<", 1)
			},
			want: "the feed advertises 1.0.2 but the build is 1.0.3",
		},
		{
			name: "an archive that is not in build",
			breakIt: func(t *testing.T, feed, archive string) string {
				return strings.Replace(feed, "MyMicroTunnel-1.0.3.zip", "elsewhere.zip", 1)
			},
			want: "the feed points at elsewhere.zip",
		},
		{
			name: "a wrong length",
			breakIt: func(t *testing.T, feed, archive string) string {
				writeFile(t, archive, readText(t, archive)+"trailing bytes")
				return feed
			},
			want: "Sparkle refuses a length",
		},
		{
			name:    "a signature that does not verify",
			verify:  true,
			breakIt: func(t *testing.T, feed, archive string) string { return feed },
			want:    "does not verify against the archive:\nERROR! Signature verification failed",
		},
		{
			name: "an archive that is not a zip",
			breakIt: func(t *testing.T, feed, archive string) string {
				size := len(readText(t, archive))
				writeFile(t, archive, strings.Repeat("x", size))
				return feed
			},
			want: "cannot be read as a zip",
		},
		{
			name: "a second package",
			breakIt: func(t *testing.T, feed, archive string) string {
				return rezip(t, feed, archive, map[string]string{
					"MyMicroTunnel-1.0.3.pkg":   "package",
					"._MyMicroTunnel-1.0.3.pkg": "AppleDouble",
					"__MACOSX/x.pkg":            "metadata",
				})
			},
			want: "the archive holds 2 packages",
		},
		{
			name: "the package one directory down",
			breakIt: func(t *testing.T, feed, archive string) string {
				return rezip(t, feed, archive, map[string]string{"build/MyMicroTunnel-1.0.3.pkg": "package"})
			},
			want: `the package at "build/MyMicroTunnel-1.0.3.pkg"`,
		},
		{
			name:      "an app without a public key",
			infoPlist: "<key>SUFeedURL</key><string>https://d.example.com/appcast.xml</string>",
			breakIt:   func(t *testing.T, feed, archive string) string { return feed },
			want:      "The built app has no SUPublicEDKey",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			infoPlist := builtInfoPlist
			if test.infoPlist != "" {
				infoPlist = test.infoPlist
			}
			root := newBuild(t, infoPlist)
			feed, archive := validFeed(t, root)
			if broken := test.breakIt(t, feed, archive); broken != "" {
				writeFile(t, filepath.Join(root, "build", "appcast.xml"), broken)
			}
			sparkle{verifyFails: test.verify}.install(t)

			code, _, stderr := runMain(t, "--validate")
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q, want %q", code, stderr, test.want)
			}
		})
	}
}

// rezip replaces the archive with files and returns the feed with its length
// corrected, so only the layout is wrong.
func rezip(t *testing.T, feed, archive string, files map[string]string) string {
	t.Helper()
	before, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	writeZip(t, archive, files)
	after, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(feed, fmt.Sprintf(`length="%d"`, before.Size()), fmt.Sprintf(`length="%d"`, after.Size()), 1)
}

func TestTheFeedURLIsOnlyTakenFromAWellFormedEntry(t *testing.T) {
	for _, test := range []struct {
		name, plist, want string
	}{
		{"present", builtInfoPlist, "https://downloads.example.com/appcast.xml"},
		{"no key", "<key>SUPublicEDKey</key><string>x</string>", ""},
		{"no value", "<key>SUFeedURL</key>", ""},
		{"an unterminated value", "<key>SUFeedURL</key><string>https://d.example.com", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "menubar", "build", "MyMicroTunnel.app", "Contents", "Info.plist"), test.plist)
			if got := feedURLFromBundle(root); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestAttributeReadsOnlyACompleteValue(t *testing.T) {
	if got := attribute(`length="12`, "length"); got != "" {
		t.Errorf("an unterminated value gave %q", got)
	}
}

func TestOutsideARepositoryNothingRuns(t *testing.T) {
	t.Chdir(t.TempDir())
	tools := sparkle{}.install(t)

	code, _, stderr := runMain(t, "--validate")
	if code != 1 || !strings.Contains(stderr, "Could not find the repository root.") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if len(tools.calls) != 0 {
		t.Errorf("ran tools: %v", tools.lines())
	}
}

func TestARunThatCannotStartIsAFailure(t *testing.T) {
	if got := run(filepath.Join(t.TempDir(), "no-such-tool")); got.code != -1 {
		t.Errorf("a missing tool gave code %d, want -1", got.code)
	}
}
