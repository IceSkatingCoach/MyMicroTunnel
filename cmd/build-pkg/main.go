// SPDX-License-Identifier: GPL-3.0-or-later
// Command build-pkg produces the distributable installer package.
//
//	go run ./cmd/build-pkg [--notarize <keychain-profile>]
//
// Signing identities are discovered from the keychain: a "Developer ID
// Application" identity signs the app and the engine binary, and a "Developer
// ID Installer" identity signs the package. A missing identity is reported
// rather than fatal, so the package can still be built and tested before the
// certificates exist.
//
// An "Apple Distribution" identity is not a substitute. That one is for the App
// Store and TestFlight, and Gatekeeper rejects it for direct downloads.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	appName    = "MyMicroTunnel.app"
	bundleID   = "ca.maragato.mymicrotunnel"
	supportDir = "/usr/local/lib/mymicrotunnel"

	// The copy the sudoers rule names. Separate from the one on the path
	// because /usr/local/bin is a directory Homebrew takes ownership of on
	// Intel, and a NOPASSWD rule pointing at a user-writable file is a
	// password-free root shell. See internal/setup.HelperPath.
	helperDir  = "/Library/PrivilegedHelperTools"
	helperName = "ca.maragato.mymicrotunnel.helper"
)

var identityPattern = regexp.MustCompile(`"([^"]+)"`)

func main() {
	notarizeProfile := flag.String("notarize", "", "notarytool keychain profile")
	flag.Parse()

	root := repoRoot()
	buildDir := filepath.Join(root, "build")
	version := appVersion(root)
	// The app is built through the Makefile, which stamps this same version
	// into the binary and into the bundle's Info.plist.
	os.Setenv("VERSION", version)

	fmt.Printf("Building MyMicroTunnel %s\n", version)
	must(os.MkdirAll(buildDir, 0o755))

	buildApp(root)

	appPath := filepath.Join(root, "menubar", "build", appName)
	resources := filepath.Join(appPath, "Contents", "Resources")
	enginePath := filepath.Join(resources, "mymicrotunnel")
	wireguardPath := filepath.Join(resources, "wireguard-go")
	verifyUniversal(
		filepath.Join(appPath, "Contents", "MacOS", "MyMicroTunnel"),
		enginePath,
		wireguardPath,
	)
	signed := signApp(appPath, append(sparkleParts(appPath), enginePath, wireguardPath)...)

	payloadRoot := stagePayload(root, buildDir, appPath)
	scriptsDir := stageScripts(buildDir)
	buildComponent(buildDir, payloadRoot, scriptsDir, version)
	packagePath := buildProduct(buildDir, version, signed)

	if *notarizeProfile != "" {
		if !signed {
			fail("Notarization needs a Developer ID signature; nothing to submit.")
		}
		notarize(packagePath, *notarizeProfile)
	}

	assess(packagePath)

	fmt.Printf("\n✓ %s\n", packagePath)
	if !signed {
		printCertificateHelp()
	}
}

// --- steps -----------------------------------------------------------------

func buildApp(root string) {
	step("Building the app and the engine")
	if code := runInteractive("make", "-C", filepath.Join(root, "menubar"), "app"); code != 0 {
		fail("The app did not build.")
	}
	done("Built")
}

// sparkleParts lists the framework's own nested code, innermost first.
//
// Sparkle is not one binary: it carries two XPC services, a helper app and the
// Autoupdate tool, and codesign seals each container as it signs it. Signing
// only the framework leaves those inner pieces ad-hoc signed, the outer seal
// then does not match them, and notarization rejects the package. `codesign
// --deep` would reach them but is documented as the wrong tool for exactly this
// case, because it also re-signs things that should have been left alone.
func sparkleParts(appPath string) []string {
	framework := filepath.Join(appPath, "Contents", "Frameworks", "Sparkle.framework")
	if _, err := os.Stat(framework); err != nil {
		return nil
	}
	versions := filepath.Join(framework, "Versions", "B")

	var parts []string
	for _, relative := range []string{
		filepath.Join("XPCServices", "Downloader.xpc"),
		filepath.Join("XPCServices", "Installer.xpc"),
		"Updater.app",
		"Autoupdate",
	} {
		candidate := filepath.Join(versions, relative)
		if _, err := os.Stat(candidate); err == nil {
			parts = append(parts, candidate)
		}
	}
	return append(parts, framework)
}

func signApp(appPath string, nested ...string) bool {
	step("Signing")

	identity := findIdentity("Developer ID Application")
	if identity == "" {
		warn("No \"Developer ID Application\" identity in the keychain; leaving the ad-hoc signature.")
		return false
	}

	// The nested binaries are signed first: codesign seals the bundle's
	// contents, so re-signing an inner file afterwards would invalidate the
	// outer signature.
	for _, target := range append(nested, appPath) {
		result := run("codesign",
			"--force", "--options", "runtime", "--timestamp", "--sign", identity, target)
		if result.code != 0 {
			fail("codesign failed on %s:\n%s", target, result.output)
		}
	}

	done("Signed with %s", identity)
	return true
}

func stagePayload(root, buildDir, appPath string) string {
	step("Staging the package payload")

	payloadRoot := filepath.Join(buildDir, "pkgroot")

	// A previous payload is not always this user's to delete. Installing the
	// package, or running the installer as root from a checkout, can leave a
	// root-owned copy of the app under build/, and then every later build
	// fails on "permission denied" for a directory nobody remembers making.
	//
	// Staging somewhere new is both the fix and the safer habit: the payload
	// is then exactly what this run put there, with no chance of shipping a
	// file left behind by an older one.
	if err := os.RemoveAll(payloadRoot); err != nil {
		payloadRoot = filepath.Join(buildDir, fmt.Sprintf("pkgroot-%d", os.Getpid()))
		warn("%s could not be removed (%v); staging in %s instead",
			filepath.Join(buildDir, "pkgroot"), err, filepath.Base(payloadRoot))
		must(os.RemoveAll(payloadRoot))
	}
	must(os.MkdirAll(filepath.Join(payloadRoot, "Applications"), 0o755))
	must(os.MkdirAll(filepath.Join(payloadRoot, supportDir[1:]), 0o755))
	must(os.MkdirAll(filepath.Join(payloadRoot, "usr/local/bin"), 0o755))

	must(os.MkdirAll(filepath.Join(payloadRoot, helperDir[1:]), 0o755))

	copyTree(appPath, filepath.Join(payloadRoot, "Applications", appName))

	resources := filepath.Join(appPath, "Contents", "Resources")

	// Three copies of one file, on purpose. The CLI on the path is for people;
	// the helper is the only one the sudoers rule names, and it lives where a
	// package manager will not hand ownership of it to the user; the one inside
	// the bundle is what makes the app self-contained. All three are the same
	// signed build, so a terminal install and a setup-window install run
	// identical code.
	engine := filepath.Join(resources, "mymicrotunnel")
	copyFile(engine, filepath.Join(payloadRoot, "usr/local/bin/mymicrotunnel"), 0o755)
	copyFile(engine, filepath.Join(payloadRoot, helperDir[1:], helperName), 0o755)

	// wireguard-go outside the bundle too, so the helper and the supervisor
	// keep working if the app is dragged to the Trash.
	copyFile(
		filepath.Join(resources, "wireguard-go"),
		filepath.Join(payloadRoot, supportDir[1:], "wireguard-go"),
		0o755,
	)

	must(os.WriteFile(
		filepath.Join(payloadRoot, supportDir[1:], "README"),
		[]byte("Installed by MyMicroTunnel.\n\n"+
			"The CloudFormation template is embedded in the binary; run\n"+
			"`mymicrotunnel install --domain <hostname>` to deploy.\n\n"+
			"wireguard-go beside this file is MIT-licensed and unmodified except for\n"+
			"a dependency upgrade needed to build it with a current Go; see\n"+
			"cmd/fetch-wireguard in the source for the exact versions.\n"),
		0o644,
	))

	// Extended attributes picked up from the build machine — quarantine flags,
	// provenance — are flattened by pkgbuild into ._ files that then sit next to
	// every binary in /Applications. The signature lives in the Mach-O, not in
	// an xattr, so there is nothing here worth carrying.
	if result := run("xattr", "-cr", payloadRoot); result.code != 0 {
		warn("Could not clear extended attributes: %s", result.output)
	}

	done("/Applications/%s, %s, %s/%s", appName, "/usr/local/bin/mymicrotunnel", helperDir, helperName)
	return payloadRoot
}

func stageScripts(buildDir string) string {
	scriptsDir := filepath.Join(buildDir, "scripts")
	must(os.RemoveAll(scriptsDir))
	must(os.MkdirAll(scriptsDir, 0o755))

	// Runs as root at the end of installation, and deliberately configures
	// nothing: the deployment needs the user's AWS credentials and their
	// consent for the privileged step, neither of which exist here. It only
	// opens the app, whose first run is the setup window.
	// Everything that was already running is still running the old code: the
	// payload has been replaced on disk, and nothing on macOS reloads a
	// process because its file changed. Three things need saying so.
	//
	//   · the menu bar app, whose bundle has just been swapped underneath it;
	//   · the supervisor daemon, which goes on executing the binary it
	//     started with until launchd restarts it;
	//   · wireguard-go, which is neither, and would carry packets through the
	//     old engine until the tunnel is bounced.
	//
	// Bouncing the tunnel means a second of downtime during an update, which
	// is a fair price for software that is actually running the version it
	// reports.
	postinstall := `#!/bin/sh
consoleUser=$(/usr/bin/stat -f%Su /dev/console)

/usr/bin/pkill -f '/Applications/MyMicroTunnel.app/Contents/MacOS/MyMicroTunnel' 2>/dev/null

if [ -f /Library/LaunchDaemons/ca.maragato.mymicrotunnel.supervisor.plist ]; then
  /bin/launchctl kickstart -k system/ca.maragato.mymicrotunnel.supervisor 2>/dev/null
fi

if [ "$consoleUser" != "root" ] && [ -n "$consoleUser" ]; then
  uid=$(/usr/bin/id -u "$consoleUser")
  home=$(/usr/bin/dscl . -read /Users/"$consoleUser" NFSHomeDirectory | /usr/bin/awk '{print $2}')
  HOME="$home" /usr/local/bin/mymicrotunnel reload 2>/dev/null
  /bin/launchctl asuser "$uid" /usr/bin/sudo -u "$consoleUser" \
    /usr/bin/open -a /Applications/MyMicroTunnel.app
fi
exit 0
`
	must(os.WriteFile(filepath.Join(scriptsDir, "postinstall"), []byte(postinstall), 0o755))
	return scriptsDir
}

func buildComponent(buildDir, payloadRoot, scriptsDir, version string) {
	step("Building the component package")

	result := run("pkgbuild",
		"--root", payloadRoot,
		"--identifier", bundleID,
		"--version", version,
		"--scripts", scriptsDir,
		"--install-location", "/",
		// Everything outside /Applications has to land as root:wheel. The
		// helper's whole reason for existing is that the user cannot write it,
		// and payload files built by a developer account are owned by that
		// account until this says otherwise.
		"--ownership", "recommended",
		filepath.Join(buildDir, "component.pkg"),
	)
	if result.code != 0 {
		fail("pkgbuild failed:\n%s", result.output)
	}
	done("component.pkg")
}

func buildProduct(buildDir, version string, appSigned bool) string {
	step("Building the distribution package")

	distribution := strings.Join([]string{
		`<?xml version="1.0" encoding="utf-8"?>`,
		`<installer-gui-script minSpecVersion="2">`,
		`  <title>MyMicroTunnel Mini VPN</title>`,
		`  <options customize="never" require-scripts="false" hostArchitectures="arm64,x86_64"/>`,
		`  <volume-check>`,
		`    <allowed-os-versions><os-version min="13.0"/></allowed-os-versions>`,
		`  </volume-check>`,
		fmt.Sprintf(`  <pkg-ref id="%s"/>`, bundleID),
		`  <choices-outline>`,
		`    <line choice="default"/>`,
		`  </choices-outline>`,
		`  <choice id="default" visible="false">`,
		fmt.Sprintf(`    <pkg-ref id="%s"/>`, bundleID),
		`  </choice>`,
		fmt.Sprintf(`  <pkg-ref id="%s" version="%s" onConclusion="none">component.pkg</pkg-ref>`, bundleID, version),
		`  <conclusion-text>`,
		`    MyMicroTunnel is installed. Its setup window opens automatically and walks`,
		`    through deploying the AWS side. You can reopen it later from the menu`,
		`    bar icon, or run: mymicrotunnel`,
		`  </conclusion-text>`,
		`</installer-gui-script>`,
		"",
	}, "\n")

	distributionPath := filepath.Join(buildDir, "distribution.xml")
	must(os.WriteFile(distributionPath, []byte(distribution), 0o644))

	output := filepath.Join(buildDir, fmt.Sprintf("MyMicroTunnel-%s.pkg", version))
	must(os.RemoveAll(output))

	arguments := []string{
		"--distribution", distributionPath,
		"--package-path", buildDir,
		output,
	}
	identity := findIdentity("Developer ID Installer")
	if identity != "" {
		arguments = append(arguments, "--sign", identity)
	}

	if result := run("productbuild", arguments...); result.code != 0 {
		fail("productbuild failed:\n%s", result.output)
	}

	if identity != "" {
		done("Signed with %s", identity)
	} else {
		warn("No \"Developer ID Installer\" identity in the keychain; the package is unsigned.")
		if appSigned {
			warn("The app inside it is signed, but Gatekeeper judges the package too.")
		}
	}

	done("%s", output)
	return output
}

func notarize(packagePath, keychainProfile string) {
	step("Notarizing")

	// Checked first, because notarytool otherwise uploads the whole package
	// before it discovers the credential is missing.
	check := run("xcrun", "notarytool", "history", "--keychain-profile", keychainProfile)
	if check.code != 0 && strings.Contains(check.output, "No Keychain password item") {
		fail("No notarytool credential named %q. Create it once with:\n\n"+
			"    xcrun notarytool store-credentials %s \\\n"+
			"      --apple-id <your-apple-id> \\\n"+
			"      --team-id <team-id> \\\n"+
			"      --password <app-specific-password>\n\n"+
			"  The app-specific password comes from appleid.apple.com under\n"+
			"  Sign-In and Security › App-Specific Passwords.",
			keychainProfile, keychainProfile)
	}

	if code := runInteractive("xcrun", "notarytool", "submit", packagePath,
		"--keychain-profile", keychainProfile, "--wait"); code != 0 {
		fail("notarytool rejected the submission. Its log URL above says why.")
	}

	// Stapling attaches the ticket to the file, so it verifies on a machine
	// that is offline or behind a filtering proxy.
	if result := run("xcrun", "stapler", "staple", packagePath); result.code != 0 {
		fail("stapler failed:\n%s", result.output)
	}
	done("Notarized and stapled")
}

func assess(packagePath string) {
	step("Verifying")
	// spctl exits non-zero for anything unsigned or unnotarized, which is
	// already reported; its own message is more useful than a second warning.
	fmt.Printf("  %s\n", run("spctl", "--assess", "--type", "install", "-vv", packagePath).output)
}

func printCertificateHelp() {
	fmt.Println("\n  To sign this for distribution you need two certificates this machine")
	fmt.Println("  does not have. An \"Apple Distribution\" identity is for the App Store")
	fmt.Println("  and will not do.")
	fmt.Println("\n  In Xcode: Settings › Accounts › your Apple ID › Manage Certificates › +")
	fmt.Println("    · Developer ID Application   (signs the app and the engine)")
	fmt.Println("    · Developer ID Installer     (signs the package)")
}

// --- plumbing --------------------------------------------------------------

type result struct {
	code   int
	output string
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
	return result{code: code, output: strings.TrimSpace(string(output))}
}

func runInteractive(name string, args ...string) int {
	command := exec.Command(name, args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if code := command.ProcessState.ExitCode(); code != 0 {
			return code
		}
		return -1
	}
	return 0
}

// findIdentity matches by prefix, because the rest of the line carries the
// account name and team id, which differ per machine.
func findIdentity(prefix string) string {
	for _, line := range strings.Split(run("security", "find-identity", "-v").output, "\n") {
		match := identityPattern.FindStringSubmatch(line)
		if len(match) == 2 && strings.HasPrefix(match[1], prefix) {
			return match[1]
		}
	}
	return ""
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

// appVersion reads the one file that says what this release is called. The
// bundle's Info.plist is generated from it, so reading the plist here would be
// reading back what the build just wrote.
func appVersion(root string) string {
	content, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		fail("No VERSION file at the repository root: %v", err)
	}
	version := strings.TrimSpace(string(content))
	if version == "" {
		fail("The VERSION file is empty.")
	}
	return version
}

// verifyUniversal refuses to ship a package that only runs on the machine that
// built it. A single-slice binary installs cleanly on the other architecture
// and then fails to launch, which looks to the customer like a broken product
// rather than a wrong download.
func verifyUniversal(paths ...string) {
	step("Checking the architectures")
	for _, path := range paths {
		info := run("lipo", "-archs", path).output
		for _, wanted := range []string{"arm64", "x86_64"} {
			if !strings.Contains(info, wanted) {
				fail("%s has no %s slice (lipo reports %q). Run `make clean && make app`.",
					filepath.Base(path), wanted, info)
			}
		}
		done("%s: %s", filepath.Base(path), info)
	}
}

func copyTree(source, destination string) {
	must(os.RemoveAll(destination))
	// `cp -R` rather than a walk, because it preserves the symlinks and the
	// permission bits that make a signed bundle verify.
	if result := run("cp", "-R", source, destination); result.code != 0 {
		fail("Could not copy %s: %s", source, result.output)
	}
}

func copyFile(source, destination string, mode os.FileMode) {
	content, err := os.ReadFile(source)
	must(err)
	must(os.WriteFile(destination, content, mode))
	must(os.Chmod(destination, mode))
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
