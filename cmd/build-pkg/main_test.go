// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	applicationIdentity = "Developer ID Application: Example Person (TEAM123456)"
	installerIdentity   = "Developer ID Installer: Example Person (TEAM123456)"
)

// Both Developer ID identities, plus the App Store one that must never be
// picked in their place.
const allIdentities = `  1) 1111111111111111111111111111111111111111 "Apple Distribution: Example Person (TEAM123456)"
  2) 2222222222222222222222222222222222222222 "` + applicationIdentity + `"
  3) 3333333333333333333333333333333333333333 "` + installerIdentity + `"
     3 valid identities found`

type scenario struct {
	identities string
	archs      string
	// failing maps the start of a command line to the output it fails with.
	failing map[string]string
}

func (s scenario) respond(name string, args []string) (string, int) {
	line := name + " " + strings.Join(args, " ")
	for prefix, output := range s.failing {
		if strings.HasPrefix(line, prefix) {
			return output, 1
		}
	}
	switch name {
	case "security":
		return s.identities, 0
	case "lipo":
		return s.archs, 0
	case "spctl":
		return "accepted\nsource=Notarized Developer ID", 0
	}
	return "", 0
}

func signedScenario() scenario {
	return scenario{identities: allIdentities, archs: "x86_64 arm64"}
}

// newCheckout lays out a repository whose app has already been built, which is
// what `make app` would have left behind.
func newCheckout(t *testing.T, withSparkle bool) (root, app string) {
	t.Helper()
	t.Setenv("VERSION", "")
	root = newRepository(t, "1.2.3")
	app = filepath.Join(root, "menubar", "build", appName)
	writeFile(t, filepath.Join(app, "Contents", "MacOS", "MyMicroTunnel"), "menu bar binary")
	writeFile(t, filepath.Join(app, "Contents", "Resources", "mymicrotunnel"), "engine binary")
	writeFile(t, filepath.Join(app, "Contents", "Resources", "wireguard-go"), "wireguard binary")
	if withSparkle {
		versions := filepath.Join(app, "Contents", "Frameworks", "Sparkle.framework", "Versions", "B")
		for _, part := range []string{
			"XPCServices/Downloader.xpc/Contents/Info.plist",
			"XPCServices/Installer.xpc/Contents/Info.plist",
			"Updater.app/Contents/Info.plist",
			"Autoupdate",
		} {
			writeFile(t, filepath.Join(versions, part), "part")
		}
	}
	return root, app
}

func TestASignedReleaseSignsTheNestedCodeBeforeTheAppThatSealsIt(t *testing.T) {
	root, app := newCheckout(t, true)
	tools := installTools(t, signedScenario().respond)

	code, stdout, stderr := runMain(t, "--notarize", "release-profile")
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}

	versions := filepath.Join(app, "Contents", "Frameworks", "Sparkle.framework", "Versions", "B")
	wantOrder := []string{
		filepath.Join(versions, "XPCServices", "Downloader.xpc"),
		filepath.Join(versions, "XPCServices", "Installer.xpc"),
		filepath.Join(versions, "Updater.app"),
		filepath.Join(versions, "Autoupdate"),
		filepath.Join(app, "Contents", "Frameworks", "Sparkle.framework"),
		filepath.Join(app, "Contents", "Resources", "mymicrotunnel"),
		filepath.Join(app, "Contents", "Resources", "wireguard-go"),
		app,
	}
	var signed []string
	for _, call := range tools.named("codesign") {
		want := []string{"--force", "--options", "runtime", "--timestamp", "--sign", applicationIdentity}
		if !slices.Equal(call.args[:len(want)], want) {
			t.Errorf("codesign was not asked for a hardened, timestamped Developer ID signature: %s", call.line())
		}
		signed = append(signed, call.args[len(call.args)-1])
	}
	if !slices.Equal(signed, wantOrder) {
		t.Errorf("signed in the wrong order:\n got %q\nwant %q", signed, wantOrder)
	}

	productbuild := tools.named("productbuild")
	if len(productbuild) != 1 || !strings.HasSuffix(productbuild[0].line(), "--sign "+installerIdentity) {
		t.Errorf("the package was not signed with the installer identity: %v", tools.lines())
	}

	packagePath := filepath.Join(root, "build", "MyMicroTunnel-1.2.3.pkg")
	xcrun := tools.named("xcrun")
	want := []string{
		"xcrun notarytool history --keychain-profile release-profile",
		"xcrun notarytool submit " + packagePath + " --keychain-profile release-profile --wait",
		"xcrun stapler staple " + packagePath,
	}
	var got []string
	for _, call := range xcrun {
		got = append(got, call.line())
	}
	if !slices.Equal(got, want) {
		t.Errorf("notarization ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	spctl := tools.named("spctl")
	if len(spctl) != 1 || spctl[0].args[len(spctl[0].args)-1] != packagePath {
		t.Errorf("the finished package was not assessed: %v", tools.lines())
	}
	if !strings.Contains(stdout, "source=Notarized Developer ID") {
		t.Errorf("spctl's verdict is not shown:\n%s", stdout)
	}
	if strings.Contains(stdout, "Manage Certificates") {
		t.Errorf("a signed build printed the certificate help:\n%s", stdout)
	}
}

func TestThePayloadPutsTheEngineWhereTheSudoersRuleAndThePathExpectIt(t *testing.T) {
	root, app := newCheckout(t, false)
	tools := installTools(t, signedScenario().respond)

	if code, _, stderr := runMain(t); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}

	if got := os.Getenv("VERSION"); got != "1.2.3" {
		t.Errorf("the Makefile is not handed the version: VERSION=%q", got)
	}
	if make := tools.named("make"); len(make) != 1 ||
		make[0].line() != "make -C "+filepath.Join(root, "menubar")+" app" {
		t.Errorf("the app was not built through the Makefile: %v", tools.lines())
	}

	payload := filepath.Join(root, "build", "pkgroot")
	for _, copy := range []struct{ path, content string }{
		{"usr/local/bin/mymicrotunnel", "engine binary"},
		{"Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper", "engine binary"},
		{"usr/local/lib/mymicrotunnel/wireguard-go", "wireguard binary"},
	} {
		path := filepath.Join(payload, copy.path)
		if got := readText(t, path); got != copy.content {
			t.Errorf("%s holds %q, want %q", copy.path, got, copy.content)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s is %v, want executable", copy.path, info.Mode().Perm())
		}
	}
	if readme := readText(t, filepath.Join(payload, "usr/local/lib/mymicrotunnel/README")); !strings.Contains(readme, "MIT-licensed") {
		t.Errorf("the README does not carry the wireguard-go licence note:\n%s", readme)
	}

	if cp := tools.named("cp"); len(cp) != 1 ||
		!slices.Equal(cp[0].args, []string{"-R", app, filepath.Join(payload, "Applications", appName)}) {
		t.Errorf("the app was not copied into /Applications with cp -R: %v", tools.lines())
	}
	if xattr := tools.named("xattr"); len(xattr) != 1 || !slices.Equal(xattr[0].args, []string{"-cr", payload}) {
		t.Errorf("extended attributes were not cleared from the payload: %v", tools.lines())
	}

	postinstall := filepath.Join(root, "build", "scripts", "postinstall")
	info, err := os.Stat(postinstall)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("postinstall is %v; the installer will not run it", info.Mode().Perm())
	}
	if script := readText(t, postinstall); !strings.HasPrefix(script, "#!/bin/sh\n") ||
		!strings.Contains(script, "kickstart -k system/ca.maragato.mymicrotunnel.supervisor") {
		t.Errorf("postinstall does not restart the supervisor:\n%s", script)
	}

	pkgbuild := tools.named("pkgbuild")
	if len(pkgbuild) != 1 {
		t.Fatalf("pkgbuild ran %d times", len(pkgbuild))
	}
	wantPkgbuild := []string{
		"--root", payload,
		"--identifier", bundleID,
		"--version", "1.2.3",
		"--scripts", filepath.Join(root, "build", "scripts"),
		"--install-location", "/",
		"--ownership", "recommended",
		filepath.Join(root, "build", "component.pkg"),
	}
	if !slices.Equal(pkgbuild[0].args, wantPkgbuild) {
		t.Errorf("pkgbuild got\n%q\nwant\n%q", pkgbuild[0].args, wantPkgbuild)
	}

	distribution := readText(t, filepath.Join(root, "build", "distribution.xml"))
	if !strings.Contains(distribution, `<pkg-ref id="ca.maragato.mymicrotunnel" version="1.2.3" onConclusion="none">component.pkg</pkg-ref>`) {
		t.Errorf("the distribution does not name this version's component:\n%s", distribution)
	}
	if !strings.Contains(distribution, `hostArchitectures="arm64,x86_64"`) {
		t.Errorf("the distribution does not allow both architectures:\n%s", distribution)
	}
	if len(tools.named("xcrun")) != 0 {
		t.Errorf("notarized without --notarize: %v", tools.lines())
	}
}

func TestWithoutCertificatesThePackageIsBuiltUnsignedAndSaysWhatIsMissing(t *testing.T) {
	newCheckout(t, true)
	tools := installTools(t, scenario{identities: "     0 valid identities found", archs: "arm64 x86_64"}.respond)

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if signed := tools.named("codesign"); len(signed) != 0 {
		t.Errorf("signed with no identity: %v", tools.lines())
	}
	if productbuild := tools.named("productbuild"); len(productbuild) != 1 || slices.Contains(productbuild[0].args, "--sign") {
		t.Errorf("productbuild was asked to sign: %v", tools.lines())
	}
	for _, want := range []string{
		`No "Developer ID Application" identity`,
		`No "Developer ID Installer" identity`,
		"Developer ID Installer     (signs the package)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "The app inside it is signed") {
		t.Errorf("claimed a signed app that is not:\n%s", stdout)
	}
}

func TestASignedAppInAnUnsignedPackageIsCalledOut(t *testing.T) {
	newCheckout(t, false)
	onlyApplication := `  1) 2222222222222222222222222222222222222222 "` + applicationIdentity + `"`
	installTools(t, scenario{identities: onlyApplication, archs: "x86_64 arm64"}.respond)

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Gatekeeper judges the package too") {
		t.Errorf("a signed app in an unsigned package went unremarked:\n%s", stdout)
	}
}

func TestNotarizingWithoutASignatureIsRefusedBeforeAnythingIsUploaded(t *testing.T) {
	newCheckout(t, false)
	tools := installTools(t, scenario{archs: "x86_64 arm64"}.respond)

	code, _, stderr := runMain(t, "--notarize", "release-profile")
	if code != 1 {
		t.Fatalf("exited %d, want 1", code)
	}
	if !strings.Contains(stderr, "Notarization needs a Developer ID signature") {
		t.Errorf("the refusal does not say why:\n%s", stderr)
	}
	if xcrun := tools.named("xcrun"); len(xcrun) != 0 {
		t.Errorf("notarytool ran anyway: %v", tools.lines())
	}
}

func TestEachToolFailureStopsTheBuildWithItsOwnMessage(t *testing.T) {
	for _, test := range []struct {
		name    string
		failing map[string]string
		archs   string
		want    string
	}{
		{
			name:    "the app does not build",
			failing: map[string]string{"make ": ""},
			want:    "The app did not build.",
		},
		{
			name:  "a binary has one slice",
			archs: "arm64",
			want:  "MyMicroTunnel has no x86_64 slice",
		},
		{
			name:    "codesign refuses",
			failing: map[string]string{"codesign ": "errSecInternalComponent"},
			want:    "errSecInternalComponent",
		},
		{
			name:    "the app cannot be copied",
			failing: map[string]string{"cp ": "Permission denied"},
			want:    "Permission denied",
		},
		{
			name:    "pkgbuild fails",
			failing: map[string]string{"pkgbuild ": "bad payload"},
			want:    "pkgbuild failed:\nbad payload",
		},
		{
			name:    "productbuild fails",
			failing: map[string]string{"productbuild ": "bad distribution"},
			want:    "productbuild failed:\nbad distribution",
		},
		{
			name:    "notarytool has no credential",
			failing: map[string]string{"xcrun notarytool history": `Error: No Keychain password item found for profile: release-profile`},
			want:    "xcrun notarytool store-credentials release-profile",
		},
		{
			name:    "notarytool rejects the package",
			failing: map[string]string{"xcrun notarytool submit": "status: Invalid"},
			want:    "notarytool rejected the submission",
		},
		{
			name:    "stapling fails",
			failing: map[string]string{"xcrun stapler": "Could not validate ticket"},
			want:    "stapler failed:\nCould not validate ticket",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			newCheckout(t, false)
			s := signedScenario()
			s.failing = test.failing
			if test.archs != "" {
				s.archs = test.archs
			}
			installTools(t, s.respond)

			code, stdout, stderr := runMain(t, "--notarize", "release-profile")
			if code != 1 {
				t.Fatalf("exited %d, want 1:\n%s", code, stdout)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("stderr does not say %q:\n%s", test.want, stderr)
			}
			if strings.Contains(stdout, "\n✓ ") {
				t.Errorf("reported success after failing:\n%s", stdout)
			}
		})
	}
}

func TestAnotherNotarytoolHistoryErrorDoesNotStopTheSubmission(t *testing.T) {
	newCheckout(t, false)
	s := signedScenario()
	s.failing = map[string]string{"xcrun notarytool history": "network timeout"}
	tools := installTools(t, s.respond)

	if code, _, stderr := runMain(t, "--notarize", "release-profile"); code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if len(tools.named("xcrun")) != 3 {
		t.Errorf("the package was not submitted and stapled: %v", tools.lines())
	}
}

func TestExtendedAttributesThatWillNotClearAreOnlyAWarning(t *testing.T) {
	newCheckout(t, false)
	s := signedScenario()
	s.failing = map[string]string{"xattr ": "Operation not permitted"}
	installTools(t, s.respond)

	code, stdout, stderr := runMain(t)
	if code != 0 {
		t.Fatalf("exited %d:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Could not clear extended attributes: Operation not permitted") {
		t.Errorf("the xattr failure was not reported:\n%s", stdout)
	}
}

func TestTheVersionFileDecidesWhetherThereIsARelease(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		want    string
	}{
		{"missing", "", "No VERSION file at the repository root"},
		{"empty", "   ", "The VERSION file is empty."},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("VERSION", "")
			root := newRepository(t, "")
			if test.version != "" {
				writeFile(t, filepath.Join(root, "VERSION"), test.version)
			}
			tools := installTools(t, signedScenario().respond)

			code, _, stderr := runMain(t)
			if code != 1 || !strings.Contains(stderr, test.want) {
				t.Errorf("exited %d with %q, want 1 and %q", code, stderr, test.want)
			}
			if len(tools.calls) != 0 {
				t.Errorf("ran tools without a version: %v", tools.lines())
			}
		})
	}
}

func TestOutsideARepositoryNothingRuns(t *testing.T) {
	t.Chdir(t.TempDir())
	tools := installTools(t, signedScenario().respond)

	code, _, stderr := runMain(t)
	if code != 1 || !strings.Contains(stderr, "Could not find the repository root.") {
		t.Errorf("exited %d with %q", code, stderr)
	}
	if len(tools.calls) != 0 {
		t.Errorf("ran tools outside a repository: %v", tools.lines())
	}
}

func TestFindIdentityIgnoresTheAppStoreIdentity(t *testing.T) {
	installTools(t, scenario{identities: `  1) 1111 "Apple Distribution: Example Person (TEAM123456)"`}.respond)

	if got := findIdentity("Developer ID Application"); got != "" {
		t.Errorf("findIdentity picked %q", got)
	}
}
