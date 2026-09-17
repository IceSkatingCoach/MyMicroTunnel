// Builds the distributable installer package.
//
//   node packaging/build-pkg.ts [--notarize <keychain-profile>]
//
// Signing identities are discovered from the keychain. A "Developer ID
// Application" identity signs the app and a "Developer ID Installer" identity
// signs the package; both are required for a download that opens without
// Gatekeeper complaining. If either is missing the build still produces a
// working package and says exactly what is unsigned and why it matters, rather
// than failing and leaving nothing to test with.
//
// An "Apple Distribution" identity is NOT a substitute: it is for the App Store
// and TestFlight, and Gatekeeper rejects it for direct downloads.

import { spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

type RunResult = { status: number; stdout: string; stderr: string };

const repoRoot = join(import.meta.dirname, "..");
const buildDir = join(repoRoot, "build");
const payloadRoot = join(buildDir, "pkgroot");
const scriptsDir = join(buildDir, "scripts");
const appName = "XpremVpn.app";
const bundleId = "ca.maragato.xprem.vpn";

/// Where the package drops everything the setup command needs at run time.
const supportDir = "/usr/local/lib/wiregard-mini-vpn";

function run(command: string, args: string[]): RunResult {
  const result = spawnSync(command, args, { encoding: "utf8" });
  return {
    status: result.status ?? -1,
    stdout: (result.stdout ?? "").trim(),
    stderr: (result.stderr ?? "").trim(),
  };
}

function runInteractive(command: string, args: string[]): number {
  return spawnSync(command, args, { stdio: "inherit" }).status ?? -1;
}

function fail(message: string): never {
  console.error(`\n✗ ${message}`);
  process.exit(1);
}

function step(message: string): void {
  console.log(`\n▸ ${message}`);
}

function done(message: string): void {
  console.log(`  ✓ ${message}`);
}

function warn(message: string): void {
  console.log(`  ! ${message}`);
}

/// Identities are matched by prefix because the rest of the line carries the
/// account name and team id, which differ per machine.
function findIdentity(prefix: string): string | undefined {
  const listing = run("security", ["find-identity", "-v"]).stdout;
  for (const line of listing.split("\n")) {
    const match = line.match(/"([^"]+)"/);
    if (match && match[1].startsWith(prefix)) return match[1];
  }
  return undefined;
}

function appVersion(): string {
  const plist = readFileSync(join(repoRoot, "menubar", "Info.plist"), "utf8");
  const match = plist.match(/<key>CFBundleShortVersionString<\/key>\s*<string>([^<]+)<\/string>/);
  return match ? match[1] : "1.0";
}

function buildApp(): void {
  step("Building the app");
  if (runInteractive("make", ["-C", join(repoRoot, "menubar"), "app"]) !== 0) {
    fail("The app did not build.");
  }
  done("Built");
}

function signApp(appPath: string): boolean {
  step("Signing the app");
  const identity = findIdentity("Developer ID Application");

  if (!identity) {
    warn("No \"Developer ID Application\" identity in the keychain; leaving the ad-hoc signature.");
    return false;
  }

  // --options runtime is the hardened runtime, which notarization requires.
  // --timestamp pins a trusted timestamp so the signature outlives the
  // certificate's expiry.
  const result = run("codesign", [
    "--force", "--options", "runtime", "--timestamp", "--sign", identity, appPath,
  ]);
  if (result.status !== 0) fail(`codesign failed:\n${result.stderr}`);

  done(`Signed with ${identity}`);
  return true;
}

function stagePayload(): void {
  step("Staging the package payload");

  rmSync(payloadRoot, { recursive: true, force: true });
  mkdirSync(join(payloadRoot, "Applications"), { recursive: true });
  mkdirSync(join(payloadRoot, supportDir.slice(1)), { recursive: true });
  mkdirSync(join(payloadRoot, "usr/local/bin"), { recursive: true });

  cpSync(join(repoRoot, "menubar", "build", appName), join(payloadRoot, "Applications", appName), {
    recursive: true,
  });

  // The setup command needs the CloudFormation template and the installer at
  // run time, because the deployment happens on the user's machine with the
  // user's credentials, not here.
  cpSync(join(repoRoot, "installer"), join(payloadRoot, supportDir.slice(1), "installer"), {
    recursive: true,
  });
  cpSync(join(repoRoot, "infra"), join(payloadRoot, supportDir.slice(1), "infra"), {
    recursive: true,
  });

  const setupCommand = [
    "#!/usr/bin/env node",
    "// Installed by the wiregard_mini_vpn package. Runs the deployment with the",
    "// invoking user's AWS credentials and their own sudo prompts, which is why",
    "// it is a command they run rather than a postinstall script running as root.",
    'import { spawnSync } from "node:child_process";',
    `const installer = "${supportDir}/installer/" + (process.argv[2] === "uninstall" ? "uninstall.ts" : "install.ts");`,
    'const result = spawnSync(process.execPath, [installer], { stdio: "inherit" });',
    "process.exit(result.status ?? 1);",
    "",
  ].join("\n");

  const setupPath = join(payloadRoot, "usr/local/bin/wiregard-mini-vpn");
  writeFileSync(setupPath, setupCommand, { mode: 0o755 });

  done(`/Applications/${appName}, ${supportDir}, /usr/local/bin/wiregard-mini-vpn`);
}

function stageScripts(): void {
  rmSync(scriptsDir, { recursive: true, force: true });
  mkdirSync(scriptsDir, { recursive: true });

  // Runs as root at the end of installation. It deliberately does no
  // configuration: the deployment needs the user's AWS credentials and their
  // consent for each sudo step, neither of which exist in this context. All it
  // does is open a Terminal, as the console user, on the setup command.
  const postinstall = [
    "#!/usr/bin/env node",
    'import { spawnSync } from "node:child_process";',
    "",
    "const consoleUser = spawnSync(\"/usr/bin/stat\", [\"-f%Su\", \"/dev/console\"], { encoding: \"utf8\" })",
    "  .stdout?.trim();",
    "",
    'if (consoleUser && consoleUser !== "root") {',
    "  const uid = spawnSync(\"/usr/bin/id\", [\"-u\", consoleUser], { encoding: \"utf8\" }).stdout?.trim();",
    "  spawnSync(\"/bin/launchctl\", [",
    '    "asuser", uid ?? "", "/usr/bin/sudo", "-u", consoleUser, "/usr/bin/osascript", "-e",',
    '    \'tell application "Terminal" to do script "wiregard-mini-vpn"\',',
    '    "-e", \'tell application "Terminal" to activate\',',
    "  ]);",
    "}",
    "",
    "// Never fail the installation over this; the conclusion pane also names the",
    "// command, and the app is already installed and usable by then.",
    "process.exit(0);",
    "",
  ].join("\n");

  writeFileSync(join(scriptsDir, "postinstall"), postinstall, { mode: 0o755 });
}

function buildComponent(version: string): string {
  step("Building the component package");

  const componentPath = join(buildDir, "component.pkg");
  const result = run("pkgbuild", [
    "--root", payloadRoot,
    "--identifier", bundleId,
    "--version", version,
    "--scripts", scriptsDir,
    "--install-location", "/",
    componentPath,
  ]);
  if (result.status !== 0) fail(`pkgbuild failed:\n${result.stderr}`);

  done("component.pkg");
  return componentPath;
}

function buildProduct(version: string, appSigned: boolean): string {
  step("Building the distribution package");

  const distributionPath = join(buildDir, "distribution.xml");
  writeFileSync(
    distributionPath,
    [
      '<?xml version="1.0" encoding="utf-8"?>',
      '<installer-gui-script minSpecVersion="2">',
      "  <title>Xprem Mini VPN</title>",
      '  <options customize="never" require-scripts="false" hostArchitectures="arm64,x86_64"/>',
      "  <volume-check>",
      '    <allowed-os-versions><os-version min="13.0"/></allowed-os-versions>',
      "  </volume-check>",
      `  <pkg-ref id="${bundleId}"/>`,
      "  <choices-outline>",
      '    <line choice="default"/>',
      "  </choices-outline>",
      '  <choice id="default" visible="false">',
      `    <pkg-ref id="${bundleId}"/>`,
      "  </choice>",
      `  <pkg-ref id="${bundleId}" version="${version}" onConclusion="none">component.pkg</pkg-ref>`,
      "  <conclusion-text>",
      "    The app is installed. A Terminal window has opened on the setup command,",
      "    which deploys the AWS side and configures the tunnel. If it did not open,",
      "    run: wiregard-mini-vpn",
      "  </conclusion-text>",
      "</installer-gui-script>",
      "",
    ].join("\n"),
  );

  const identity = findIdentity("Developer ID Installer");
  const output = join(buildDir, `XpremVpn-${version}.pkg`);

  const args = [
    "--distribution", distributionPath,
    "--package-path", buildDir,
    output,
  ];
  if (identity) args.push("--sign", identity);

  rmSync(output, { force: true });
  const result = run("productbuild", args);
  if (result.status !== 0) fail(`productbuild failed:\n${result.stderr}`);

  if (identity) {
    done(`Signed with ${identity}`);
  } else {
    warn("No \"Developer ID Installer\" identity in the keychain; the package is unsigned.");
    if (appSigned) warn("The app inside it is signed, but Gatekeeper judges the package too.");
  }

  done(output);
  return output;
}

function notarize(packagePath: string, keychainProfile: string): void {
  step("Notarizing");

  const submit = runInteractive("xcrun", [
    "notarytool", "submit", packagePath, "--keychain-profile", keychainProfile, "--wait",
  ]);
  if (submit !== 0) fail("notarytool rejected the submission. Its log URL above says why.");

  // Stapling attaches the ticket to the file, so it verifies on a machine that
  // is offline or behind a filtering proxy.
  const staple = run("xcrun", ["stapler", "staple", packagePath]);
  if (staple.status !== 0) fail(`stapler failed:\n${staple.stdout}\n${staple.stderr}`);

  done("Notarized and stapled");
}

function verify(packagePath: string): void {
  step("Verifying");

  const assessment = run("spctl", ["--assess", "--type", "install", "-vv", packagePath]);
  // spctl exits non-zero for an unsigned package, which is expected and already
  // reported; its message is more useful than a second warning of our own.
  console.log(`  ${assessment.stderr || assessment.stdout}`);
}

function main(): void {
  const notarizeIndex = process.argv.indexOf("--notarize");
  const keychainProfile = notarizeIndex === -1 ? undefined : process.argv[notarizeIndex + 1];
  if (notarizeIndex !== -1 && !keychainProfile) {
    fail("--notarize needs the name of a notarytool keychain profile.");
  }

  const version = appVersion();
  console.log(`Building XpremVpn ${version}`);

  mkdirSync(buildDir, { recursive: true });
  buildApp();

  const appPath = join(repoRoot, "menubar", "build", appName);
  if (!existsSync(appPath)) fail(`The built app is missing at ${appPath}.`);
  const appSigned = signApp(appPath);

  stagePayload();
  stageScripts();
  buildComponent(version);
  const packagePath = buildProduct(version, appSigned);

  if (keychainProfile) {
    if (!appSigned) fail("Notarization needs a Developer ID signature; nothing to submit.");
    notarize(packagePath, keychainProfile);
  }

  verify(packagePath);

  console.log(`\n✓ ${packagePath}`);
  if (!appSigned) {
    console.log("\n  To sign this for distribution you need two certificates this machine");
    console.log("  does not have. The \"Apple Distribution\" identity already in the keychain");
    console.log("  is for the App Store and will not do.");
    console.log("\n  In Xcode: Settings › Accounts › your Apple ID › Manage Certificates › +");
    console.log("    · Developer ID Application   (signs the app)");
    console.log("    · Developer ID Installer     (signs the package)");
    console.log("\n  Both require the Account Holder role on team Z4FDTJHQP3.");
    console.log("  Then store a notarytool credential once:");
    console.log("    xcrun notarytool store-credentials wiregard --apple-id <id> \\");
    console.log("      --team-id Z4FDTJHQP3 --password <app-specific-password>");
    console.log("  and rebuild with: make pkg-notarized PROFILE=wiregard");
  }
}

main();
