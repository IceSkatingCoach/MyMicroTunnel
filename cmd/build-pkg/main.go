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
	appName    = "XpremVpn.app"
	bundleID   = "ca.maragato.xprem.vpn"
	supportDir = "/usr/local/lib/wiregard-mini-vpn"
)

var identityPattern = regexp.MustCompile(`"([^"]+)"`)

func main() {
	notarizeProfile := flag.String("notarize", "", "notarytool keychain profile")
	flag.Parse()

	root := repoRoot()
	buildDir := filepath.Join(root, "build")
	version := appVersion(root)

	fmt.Printf("Building XpremVpn %s\n", version)
	must(os.MkdirAll(buildDir, 0o755))

	buildApp(root)

	appPath := filepath.Join(root, "menubar", "build", appName)
	enginePath := filepath.Join(appPath, "Contents", "Resources", "wiregard-mini-vpn")
	signed := signApp(appPath, enginePath)

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

func signApp(appPath, enginePath string) bool {
	step("Signing")

	identity := findIdentity("Developer ID Application")
	if identity == "" {
		warn("No \"Developer ID Application\" identity in the keychain; leaving the ad-hoc signature.")
		return false
	}

	// The nested binary is signed first: codesign seals the bundle's contents,
	// so re-signing an inner file afterwards would invalidate the outer
	// signature.
	for _, target := range []string{enginePath, appPath} {
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
	must(os.RemoveAll(payloadRoot))
	must(os.MkdirAll(filepath.Join(payloadRoot, "Applications"), 0o755))
	must(os.MkdirAll(filepath.Join(payloadRoot, supportDir[1:]), 0o755))
	must(os.MkdirAll(filepath.Join(payloadRoot, "usr/local/bin"), 0o755))

	copyTree(appPath, filepath.Join(payloadRoot, "Applications", appName))

	// The CLI on the path and the copy inside the app bundle are the same
	// binary, so a terminal install and a setup-window install run identical
	// code.
	copyFile(
		filepath.Join(appPath, "Contents", "Resources", "wiregard-mini-vpn"),
		filepath.Join(payloadRoot, "usr/local/bin/wiregard-mini-vpn"),
		0o755,
	)

	// The template is embedded in the binary, so nothing else has to ship. The
	// directory is still created: the uninstaller and any future support files
	// expect it to exist.
	must(os.WriteFile(
		filepath.Join(payloadRoot, supportDir[1:], "README"),
		[]byte("Installed by wiregard_mini_vpn. The CloudFormation template is embedded in\n"+
			"/usr/local/bin/wiregard-mini-vpn; run `wiregard-mini-vpn install` to deploy.\n"),
		0o644,
	))

	done("/Applications/%s, /usr/local/bin/wiregard-mini-vpn", appName)
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
	postinstall := `#!/bin/sh
consoleUser=$(/usr/bin/stat -f%Su /dev/console)
if [ "$consoleUser" != "root" ] && [ -n "$consoleUser" ]; then
  uid=$(/usr/bin/id -u "$consoleUser")
  /bin/launchctl asuser "$uid" /usr/bin/sudo -u "$consoleUser" \
    /usr/bin/open -a /Applications/XpremVpn.app
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
		`  <title>Xprem Mini VPN</title>`,
		`  <options customize="never" require-scripts="false" hostArchitectures="arm64"/>`,
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
		`    Xprem VPN is installed. Its setup window opens automatically and walks`,
		`    through deploying the AWS side. You can reopen it later from the menu`,
		`    bar icon, or run: wiregard-mini-vpn`,
		`  </conclusion-text>`,
		`</installer-gui-script>`,
		"",
	}, "\n")

	distributionPath := filepath.Join(buildDir, "distribution.xml")
	must(os.WriteFile(distributionPath, []byte(distribution), 0o644))

	output := filepath.Join(buildDir, fmt.Sprintf("XpremVpn-%s.pkg", version))
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

func appVersion(root string) string {
	content, err := os.ReadFile(filepath.Join(root, "menubar", "Info.plist"))
	if err != nil {
		return "1.0"
	}
	pattern := regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]+)</string>`)
	if match := pattern.FindStringSubmatch(string(content)); len(match) == 2 {
		return match[1]
	}
	return "1.0"
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
