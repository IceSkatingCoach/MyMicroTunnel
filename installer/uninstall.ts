// Reverses installer/install.ts. Run with `node installer/uninstall.ts`.
//
// Local state is removed without asking, because it is all re-creatable. The
// two things that are not — the AWS stack and the WireGuard private key — are
// only touched on an explicit yes.

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, rmSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline/promises";

const configDir = join(homedir(), "Library", "Application Support", "XpremVpn");
const configPath = join(configDir, "config.json");
const sudoersPath = "/etc/sudoers.d/xprem-vpn";
const installedApp = "/Applications/XpremVpn.app";

const rl = createInterface({ input: process.stdin, output: process.stdout });

function run(command: string, args: string[]): { status: number; stdout: string } {
  const result = spawnSync(command, args, { encoding: "utf8" });
  return { status: result.status ?? -1, stdout: (result.stdout ?? "").trim() };
}

function runInteractive(command: string, args: string[]): number {
  return spawnSync(command, args, { stdio: "inherit" }).status ?? -1;
}

function done(message: string): void {
  console.log(`  ✓ ${message}`);
}

async function confirm(question: string, fallback: boolean): Promise<boolean> {
  const hint = fallback ? "Y/n" : "y/N";
  const answer = (await rl.question(`  ${question} (${hint}): `)).trim().toLowerCase();
  if (answer.length === 0) return fallback;
  return answer.startsWith("y");
}

type Config = {
  interfaceName?: string;
  wgQuickPath?: string;
};

function readConfig(): Config {
  if (!existsSync(configPath)) return {};
  try {
    return JSON.parse(readFileSync(configPath, "utf8")) as Config;
  } catch {
    return {};
  }
}

async function main(): Promise<void> {
  const config = readConfig();
  const interfaceName = config.interfaceName ?? "wg0";
  const wgQuickPath = config.wgQuickPath ?? "/opt/homebrew/bin/wg-quick";

  console.log("wiregard_mini_vpn uninstaller\n");

  // Down first: removing the sudoers rule would take away the means to do it.
  runInteractive("/usr/bin/sudo", [wgQuickPath, "down", interfaceName]);
  done("Tunnel down");

  run("/usr/bin/pkill", ["-f", "XpremVpn.app/Contents/MacOS/XpremVpn"]);
  run("osascript", [
    "-e",
    'tell application "System Events" to delete (every login item whose name is "XpremVpn")',
  ]);
  rmSync(installedApp, { recursive: true, force: true });
  done("App and login item removed");

  runInteractive("/usr/bin/sudo", ["rm", "-f", sudoersPath]);
  done("Sudoers rule removed");

  rmSync(configDir, { recursive: true, force: true });
  done("App configuration removed");

  if (await confirm("Also delete /etc/wireguard (tunnel config and private key)?", false)) {
    runInteractive("/usr/bin/sudo", ["rm", "-rf", "/etc/wireguard"]);
    done("/etc/wireguard removed");
  }

  if (await confirm("Also delete the AWS CloudFormation stack?", false)) {
    const stackName = (await rl.question("  Stack name [xprem-onprem-vpn]: ")).trim()
      || "xprem-onprem-vpn";
    const profile = (await rl.question("  AWS profile [default]: ")).trim() || "default";
    const region = (await rl.question("  Region [us-east-2]: ")).trim() || "us-east-2";

    console.log(`\n  Deleting ${stackName}. This takes down the hostname it serves.`);
    if (await confirm(`Delete ${stackName} in ${region}?`, false)) {
      runInteractive("aws", [
        "cloudformation", "delete-stack",
        "--stack-name", stackName, "--profile", profile, "--region", region,
      ]);
      runInteractive("aws", [
        "cloudformation", "wait", "stack-delete-complete",
        "--stack-name", stackName, "--profile", profile, "--region", region,
      ]);
      done("Stack deleted");
    }
  }

  console.log("\n✓ Uninstalled.");
  rl.close();
}

main().catch((error: unknown) => {
  console.error(`\n✗ ${error instanceof Error ? error.message : String(error)}`);
  process.exit(1);
});
