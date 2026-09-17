// End-to-end installer for the mini VPN: it collects AWS credentials, deploys
// the CloudFormation stack, generates the WireGuard keys, writes the tunnel and
// sudoers files that need root, builds and installs the menu bar app, and then
// proves the whole path works before it claims success.
//
// Run with `node installer/install.ts` — Node 22.6+ strips the types on its
// own, so there is no build step and no dependency to install.
//
// Written to be re-runnable. Every step checks for its own result first, so a
// second run after a failure resumes rather than duplicating work.

import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { homedir, tmpdir, userInfo } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline/promises";

type RunResult = {
  status: number;
  stdout: string;
  stderr: string;
};

type Settings = {
  profile: string;
  region: string;
  stackName: string;
  domainName: string;
  dnsStackName: string;
  servicePort: string;
  interfaceName: string;
  clientAddress: string;
  gatewayAddress: string;
  wgQuickPath: string;
};

const repoRoot = join(import.meta.dirname, "..");
const templatePath = join(repoRoot, "infra", "cloudformation-xprem-onprem-vpn.yaml");
const menubarDir = join(repoRoot, "menubar");
const appName = "XpremVpn.app";
const installedApp = join("/Applications", appName);
const configDir = join(homedir(), "Library", "Application Support", "XpremVpn");
const configPath = join(configDir, "config.json");
const sudoersPath = "/etc/sudoers.d/xprem-vpn";
const clientKeyPath = "/etc/wireguard/client.key";

const rl = createInterface({ input: process.stdin, output: process.stdout });

// --- plumbing --------------------------------------------------------------

function run(command: string, args: string[], input?: string): RunResult {
  const result = spawnSync(command, args, { encoding: "utf8", input });
  return {
    status: result.status ?? -1,
    stdout: (result.stdout ?? "").trim(),
    stderr: (result.stderr ?? "").trim(),
  };
}

/// Streams straight to the terminal, for the long ones: CloudFormation deploys,
/// Homebrew installs, and anything that may ask for a password.
function runInteractive(command: string, args: string[]): number {
  const result = spawnSync(command, args, { stdio: "inherit" });
  return result.status ?? -1;
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

async function ask(question: string, fallback: string): Promise<string> {
  const answer = (await rl.question(`  ${question} [${fallback}]: `)).trim();
  return answer.length > 0 ? answer : fallback;
}

async function confirm(question: string, fallback: boolean): Promise<boolean> {
  const hint = fallback ? "Y/n" : "y/N";
  const answer = (await rl.question(`  ${question} (${hint}): `)).trim().toLowerCase();
  if (answer.length === 0) return fallback;
  return answer.startsWith("y");
}

/// Reads without echoing. Secret access keys should not end up in the scrollback
/// of a shared terminal, or in a screen recording.
async function askSecret(question: string): Promise<string> {
  process.stdout.write(`  ${question}: `);
  const previouslyRaw = process.stdin.isRaw ?? false;
  process.stdin.setRawMode?.(true);

  const secret = await new Promise<string>((resolve) => {
    let buffer = "";
    const onData = (chunk: Buffer) => {
      const text = chunk.toString("utf8");
      for (const character of text) {
        if (character === "\r" || character === "\n") {
          process.stdin.off("data", onData);
          process.stdin.setRawMode?.(previouslyRaw);
          process.stdout.write("\n");
          resolve(buffer);
          return;
        }
        if (character === "") {
          process.stdout.write("\n");
          process.exit(130);
        }
        if (character === "") {
          buffer = buffer.slice(0, -1);
          continue;
        }
        buffer += character;
      }
    };
    process.stdin.on("data", onData);
  });

  return secret.trim();
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/// Writes a file that must be owned by root. `sudo install` is used instead of
/// `sudo tee` so the mode and owner are set in the same step as the write,
/// never leaving a readable window on a file holding a private key.
function installAsRoot(content: string, destination: string, mode: string): void {
  const scratch = mkdtempSync(join(tmpdir(), "xprem-vpn-"));
  const staged = join(scratch, "staged");
  try {
    writeFileSync(staged, content, { mode: 0o600 });
    const status = runInteractive("/usr/bin/sudo", [
      "install", "-m", mode, "-o", "root", "-g", "wheel", staged, destination,
    ]);
    if (status !== 0) fail(`Could not write ${destination}.`);
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

function aws(settings: Settings, args: string[]): RunResult {
  return run("aws", [...args, "--profile", settings.profile, "--region", settings.region]);
}

// --- steps -----------------------------------------------------------------

function checkPrerequisites(): string {
  step("Checking prerequisites");

  if (process.platform !== "darwin") fail("This installer only supports macOS.");

  if (run("which", ["aws"]).status !== 0) {
    fail("The AWS CLI is not installed. Install it with `brew install awscli`, then re-run.");
  }
  done("AWS CLI found");

  let wgQuick = run("which", ["wg-quick"]).stdout;
  if (wgQuick.length === 0) {
    console.log("  WireGuard tools are missing; installing with Homebrew.");
    if (runInteractive("brew", ["install", "wireguard-tools"]) !== 0) {
      fail("`brew install wireguard-tools` failed.");
    }
    wgQuick = run("which", ["wg-quick"]).stdout;
    if (wgQuick.length === 0) fail("wg-quick is still not on PATH after installing.");
  }
  done(`wg-quick at ${wgQuick}`);

  if (run("which", ["swiftc"]).status !== 0) {
    fail("swiftc is missing. Install the Xcode command line tools with `xcode-select --install`.");
  }
  done("Swift compiler found");

  if (!existsSync(templatePath)) fail(`Template not found at ${templatePath}.`);

  return wgQuick;
}

async function resolveCredentials(): Promise<{ profile: string; region: string }> {
  step("AWS credentials");

  const profiles = run("aws", ["configure", "list-profiles"]).stdout
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.length > 0);

  let useExisting = false;
  if (profiles.length > 0) {
    console.log(`  Existing profiles: ${profiles.join(", ")}`);
    useExisting = await confirm("Use one of them?", true);
  }

  let profile: string;
  let region: string;

  if (useExisting) {
    profile = await ask("Profile", profiles[0]);
    if (!profiles.includes(profile)) fail(`No such profile: ${profile}`);
    const configured = run("aws", ["configure", "get", "region", "--profile", profile]).stdout;
    region = await ask("Region", configured.length > 0 ? configured : "us-east-2");
  } else {
    profile = await ask("Name for the new profile", "xprem-vpn");
    const accessKeyId = await ask("AWS access key id", "");
    if (accessKeyId.length === 0) fail("An access key id is required.");
    const secretAccessKey = await askSecret("AWS secret access key (hidden)");
    if (secretAccessKey.length === 0) fail("A secret access key is required.");
    region = await ask("Region", "us-east-2");

    // Written through the CLI rather than by editing ~/.aws/credentials, so
    // file format and permissions stay the CLI's problem, not ours.
    run("aws", ["configure", "set", "aws_access_key_id", accessKeyId, "--profile", profile]);
    run("aws", ["configure", "set", "aws_secret_access_key", secretAccessKey, "--profile", profile]);
    run("aws", ["configure", "set", "region", region, "--profile", profile]);
    done(`Profile ${profile} written to ~/.aws/credentials`);
  }

  const identity = run("aws", [
    "sts", "get-caller-identity", "--profile", profile, "--region", region, "--output", "text",
  ]);
  if (identity.status !== 0) fail(`Those credentials do not work:\n${identity.stderr}`);
  done(`Authenticated as ${identity.stdout.split("\t").pop()}`);

  return { profile, region };
}

async function collectSettings(wgQuickPath: string): Promise<Settings> {
  const { profile, region } = await resolveCredentials();

  step("Deployment settings");
  const stackName = await ask("CloudFormation stack name", "xprem-onprem-vpn");
  const domainName = await ask("Public hostname", "update.mobile.maragato.ca");
  const dnsStackName = await ask("Route53 stack name that owns the zone", "xprem-dns");
  const servicePort = await ask("Port the local service listens on", "3000");
  const clientAddress = await ask("Tunnel address for this machine", "10.100.0.2");
  const gatewayAddress = await ask("Tunnel address for the AWS gateway", "10.100.0.1");

  return {
    profile,
    region,
    stackName,
    domainName,
    dnsStackName,
    servicePort,
    interfaceName: "wg0",
    clientAddress,
    gatewayAddress,
    wgQuickPath,
  };
}

/// The private key is generated on this machine and never leaves it; only the
/// public half is handed to CloudFormation.
function ensureClientKey(): string {
  step("WireGuard client key");

  const existing = run("/usr/bin/sudo", ["cat", clientKeyPath]);
  if (existing.status === 0 && existing.stdout.length > 0) {
    done("Reusing the existing key");
    return run("wg", ["pubkey"], `${existing.stdout}\n`).stdout;
  }

  const privateKey = run("wg", ["genkey"]).stdout;
  if (privateKey.length === 0) fail("`wg genkey` produced nothing.");

  runInteractive("/usr/bin/sudo", ["mkdir", "-p", "/etc/wireguard"]);
  runInteractive("/usr/bin/sudo", ["chmod", "700", "/etc/wireguard"]);
  installAsRoot(`${privateKey}\n`, clientKeyPath, "0600");
  done(`Key written to ${clientKeyPath}`);

  return run("wg", ["pubkey"], `${privateKey}\n`).stdout;
}

function deployStack(settings: Settings, clientPublicKey: string): void {
  step(`Deploying ${settings.stackName} (this takes a few minutes)`);

  const status = runInteractive("aws", [
    "cloudformation", "deploy",
    "--stack-name", settings.stackName,
    "--template-file", templatePath,
    "--capabilities", "CAPABILITY_IAM",
    "--parameter-overrides",
    `ClientPublicKey=${clientPublicKey}`,
    `ServiceDomainName=${settings.domainName}`,
    `DnsStackName=${settings.dnsStackName}`,
    `ServicePort=${settings.servicePort}`,
    `ClientVpnAddress=${settings.clientAddress}`,
    `GatewayVpnAddress=${settings.gatewayAddress}`,
    "--profile", settings.profile,
    "--region", settings.region,
  ]);

  if (status !== 0) fail("The CloudFormation deploy failed. The output above says why.");
  done("Stack deployed");
}

function stackOutput(settings: Settings, key: string): string {
  const result = aws(settings, [
    "cloudformation", "describe-stacks",
    "--stack-name", settings.stackName,
    "--query", `Stacks[0].Outputs[?OutputKey=='${key}'].OutputValue`,
    "--output", "text",
  ]);
  if (result.status !== 0) fail(`Could not read stack output ${key}:\n${result.stderr}`);
  return result.stdout;
}

/// The gateway generates its own keypair at boot and publishes the public half,
/// so this waits on the instance rather than on CloudFormation.
async function waitForServerKey(settings: Settings): Promise<string> {
  step("Waiting for the gateway to publish its public key");

  for (let attempt = 0; attempt < 30; attempt += 1) {
    const result = aws(settings, [
      "ssm", "get-parameter",
      "--name", "/xprem/vpn/server-public-key",
      "--query", "Parameter.Value",
      "--output", "text",
    ]);
    if (result.status === 0 && result.stdout.length > 0) {
      done("Gateway key retrieved");
      return result.stdout;
    }
    await sleep(10_000);
  }

  return fail(
    "The gateway never published its key. Check the instance's /var/log/cloud-init-output.log "
      + "over SSM Session Manager.",
  );
}

function writeTunnelConfig(settings: Settings, serverPublicKey: string, endpoint: string): void {
  step("Writing the tunnel configuration");

  const config = [
    "[Interface]",
    `Address = ${settings.clientAddress}/32`,
    "MTU = 1380",
    "# Reads the key from the file the installer created, so the private key is",
    "# not duplicated into this config.",
    `PostUp = wg set %i private-key ${clientKeyPath}`,
    "",
    "[Peer]",
    `PublicKey = ${serverPublicKey}`,
    `AllowedIPs = 10.100.0.0/24, 172.31.0.0/16`,
    `Endpoint = ${endpoint}:51820`,
    "# Keeps the NAT mapping open, and re-pins the tunnel when this machine's",
    "# public address changes.",
    "PersistentKeepalive = 25",
    "",
  ].join("\n");

  installAsRoot(config, `/etc/wireguard/${settings.interfaceName}.conf`, "0600");
  done(`/etc/wireguard/${settings.interfaceName}.conf written`);
}

function installSudoersRule(settings: Settings): void {
  step("Granting the app permission to toggle the tunnel");

  const user = userInfo().username;
  const rule = [
    "# Installed by wiregard_mini_vpn. Lets the menu bar app raise and drop the",
    "# tunnel without a password prompt on every toggle.",
    "#",
    "# Scope: these two exact command lines only. This is not a general root",
    `# shell. Its safety depends on /etc/wireguard/${settings.interfaceName}.conf staying`,
    "# root-owned and mode 0600, because wg-quick runs that file's PostUp as root.",
    `${user} ALL=(root) NOPASSWD: ${settings.wgQuickPath} up ${settings.interfaceName}, `
      + `${settings.wgQuickPath} down ${settings.interfaceName}`,
    "",
  ].join("\n");

  // Validated before installation: a malformed file in /etc/sudoers.d can lock
  // every sudo on the machine, including the one needed to remove it.
  const scratch = mkdtempSync(join(tmpdir(), "xprem-sudoers-"));
  const staged = join(scratch, "xprem-vpn");
  writeFileSync(staged, rule, { mode: 0o600 });
  const check = run("/usr/sbin/visudo", ["-c", "-f", staged]);
  if (check.status !== 0) {
    rmSync(scratch, { recursive: true, force: true });
    fail(`The generated sudoers rule is invalid, so it was not installed:\n${check.stdout}\n${check.stderr}`);
  }
  rmSync(scratch, { recursive: true, force: true });

  installAsRoot(rule, sudoersPath, "0440");
  done(`${sudoersPath} installed and validated`);
}

function writeAppConfig(settings: Settings): void {
  step("Writing the app configuration");

  mkdirSync(configDir, { recursive: true });
  writeFileSync(
    configPath,
    `${JSON.stringify(
      {
        interfaceName: settings.interfaceName,
        clientAddress: settings.clientAddress,
        gatewayAddress: settings.gatewayAddress,
        wgQuickPath: settings.wgQuickPath,
        healthCheckUrl: `https://${settings.domainName}/hc`,
      },
      null,
      2,
    )}\n`,
  );
  done(configPath);
}

function buildAndInstallApp(): void {
  // When run from the installed package the app is already in /Applications and
  // the Swift sources are not shipped, so there is nothing to build.
  if (!existsSync(menubarDir)) {
    step("Menu bar app");
    if (!existsSync(installedApp)) {
      fail(`${installedApp} is missing and there are no sources to build it from.`);
    }
    done(`Already installed at ${installedApp}`);
    return;
  }

  step("Building the menu bar app");
  if (runInteractive("make", ["-C", menubarDir, "app"]) !== 0) fail("The app did not build.");

  // Replaced rather than merged, so a stale binary can never survive an upgrade.
  run("/usr/bin/pkill", ["-f", `${appName}/Contents/MacOS/XpremVpn`]);
  rmSync(installedApp, { recursive: true, force: true });
  if (run("cp", ["-R", join(menubarDir, "build", appName), "/Applications/"]).status !== 0) {
    fail(`Could not copy the app into /Applications.`);
  }
  done(installedApp);
}

async function offerLoginItem(): Promise<void> {
  step("Login item");
  if (!(await confirm("Open the app automatically at login?", true))) {
    done("Skipped");
    return;
  }

  // Opening at login only puts the switch in the menu bar; it does not raise
  // the tunnel, which stays a deliberate act.
  run("osascript", [
    "-e",
    'tell application "System Events" to delete (every login item whose name is "XpremVpn")',
  ]);
  const result = run("osascript", [
    "-e",
    'tell application "System Events" to make login item at end with properties '
      + `{path:"${installedApp}", hidden:true}`,
  ]);
  if (result.status !== 0) {
    console.log(`  ! Could not register the login item automatically: ${result.stderr}`);
    console.log("    Add it by hand under System Settings › General › Login Items.");
    return;
  }
  done("Registered");
}

async function verify(settings: Settings): Promise<void> {
  step("Verifying the whole path");

  const up = run("/usr/bin/sudo", ["-n", settings.wgQuickPath, "up", settings.interfaceName]);
  if (up.status !== 0 && !up.stderr.includes("already exists")) {
    fail(`The tunnel would not come up:\n${up.stderr}`);
  }
  done("Tunnel is up");

  const ping = run("/sbin/ping", ["-c", "2", "-t", "5", settings.gatewayAddress]);
  if (ping.status !== 0) fail(`The gateway at ${settings.gatewayAddress} did not answer.`);
  done(`Gateway ${settings.gatewayAddress} answers`);

  const targetGroup = aws(settings, [
    "elbv2", "describe-target-groups",
    "--query", "TargetGroups[?contains(TargetGroupName, 'xprem')].TargetGroupArn",
    "--output", "text",
  ]).stdout.split(/\s+/)[0];

  // Two consecutive checks 30s apart have to pass, so this can take a minute.
  let health = "unknown";
  for (let attempt = 0; attempt < 10; attempt += 1) {
    health = aws(settings, [
      "elbv2", "describe-target-health",
      "--target-group-arn", targetGroup,
      "--query", "TargetHealthDescriptions[0].TargetHealth.State",
      "--output", "text",
    ]).stdout;
    if (health === "healthy") break;
    await sleep(20_000);
  }
  if (health !== "healthy") {
    fail(
      `The load balancer still reports the target as "${health}". `
        + `Check that the service is listening on 0.0.0.0:${settings.servicePort}.`,
    );
  }
  done("Load balancer target is healthy");

  const probe = run("curl", [
    "-s", "-m", "15", "-o", "/dev/null", "-w", "%{http_code}",
    `https://${settings.domainName}/hc`,
  ]);
  if (probe.stdout !== "200") fail(`https://${settings.domainName}/hc returned ${probe.stdout}.`);
  done(`https://${settings.domainName}/hc returns 200`);
}

// --- main ------------------------------------------------------------------

async function main(): Promise<void> {
  console.log("wiregard_mini_vpn installer\n");
  console.log("This deploys AWS resources that cost roughly USD 26/month, and");
  console.log("asks for your password to write two root-owned files.");

  const wgQuickPath = checkPrerequisites();
  const settings = await collectSettings(wgQuickPath);

  console.log("\n  About to deploy:");
  console.log(`    stack     ${settings.stackName} in ${settings.region}`);
  console.log(`    hostname  https://${settings.domainName}`);
  console.log(`    target    ${settings.clientAddress}:${settings.servicePort} on this machine`);
  if (!(await confirm("Proceed?", true))) fail("Cancelled.");

  const clientPublicKey = ensureClientKey();
  deployStack(settings, clientPublicKey);

  const endpoint = stackOutput(settings, "GatewayPublicIp");
  const serverPublicKey = await waitForServerKey(settings);

  writeTunnelConfig(settings, serverPublicKey, endpoint);
  installSudoersRule(settings);
  writeAppConfig(settings);
  buildAndInstallApp();
  await offerLoginItem();
  await verify(settings);

  run("/usr/bin/open", [installedApp]);

  console.log("\n✓ Installed.");
  console.log(`  The padlock shield in the menu bar toggles ${settings.domainName}.`);
  console.log(`  Uninstall with: node installer/uninstall.ts`);
  rl.close();
}

main().catch((error: unknown) => {
  fail(error instanceof Error ? error.message : String(error));
});
