# wiregard_mini_vpn

A small WireGuard tunnel that lets an AWS Network Load Balancer publish a
service running on a workstation, plus the macOS menu bar switch that raises and
drops that tunnel on demand.

It exists because some services are better served from a laptop than from
Fargate — and that laptop should only be reachable while someone wants it to be.

```
client ──TLS──▶ NLB :443 ──▶ 10.100.0.2:3000
                              │
                      VPC route 10.100.0.0/24
                              ▼
                      gateway instance (EIP)
                              │  WireGuard
                              ▼
                      workstation 10.100.0.2 ──▶ service :3000
```

The AWS account is the customer's. Nothing in the template names a particular
account, and the installer discovers the VPC, the subnets, the route tables and
the Route53 zone rather than asking anyone to look them up.

## Why not AWS Site-to-Site VPN

An `AWS::EC2::CustomerGateway` needs a fixed public IP address, because AWS
answers the IKE negotiation rather than starting it. A workstation behind a
residential NAT does not have one. AWS does support a certificate-based customer
gateway with no address for exactly this case, but it requires an ACM Private CA
at roughly USD 400 per month, an order of magnitude more than everything else
here combined.

WireGuard inverts the direction. The workstation dials out to an Elastic IP, so
no inbound port forward is needed on the home router, and `PersistentKeepalive`
re-pins the tunnel after the ISP hands out a new address.

## Nothing to install first

The package carries its own WireGuard. macOS has no in-kernel WireGuard, so the
data plane has to run in userspace; that is `wireguard-go`, which is MIT, is
built from a pinned and checksummed release by `make wireguard`, and ships inside
the package.

`wg(8)` and `wg-quick(8)` are not shipped and not needed. `wg-quick` on macOS is
a bash script that needs bash 4, which macOS has not had since 2007, and
`wireguard-tools` is GPLv2 — between them that meant depending on Homebrew, on a
second installer, and on a licence this package would otherwise have to carry.
Everything they did here is in `internal/tunnel`, which speaks wireguard-go's
[UAPI](https://www.wireguard.com/xplatform/) directly. The config file it writes
is still wg-quick's format, so the Homebrew tools can drive the same tunnel if
they happen to be installed.

See `THIRD-PARTY-NOTICES.md`.

## Two front ends, one implementation

The deployment logic lives once, in a Go binary. Everything else drives it.

| Front end | For |
| --- | --- |
| `wiregard-mini-vpn` in a terminal | scripted or headless installs |
| The app's setup window | the packaged product, where there is no terminal |

The setup window runs the same binary with `--json` and renders the NDJSON
events it emits, so the two cannot drift apart. The binary is embedded in the
app bundle and installed on the path, and it is the same file in both places.

Because the binary uses the AWS SDK directly, an installed copy depends on
nothing the user has to fetch first: no AWS CLI, no Node, no Python. The
CloudFormation template is compiled into it with `go:embed`.

## Install

From a source checkout:

```sh
make install DOMAIN=updates.example.com   # deploy and configure
make uninstall                            # remove; the stack and the private key need an explicit yes
```

From the package: open `XpremVpn.pkg`. It installs the app and opens it, and the
first run is the setup window — AWS profile or access key, hostname, then a live
log of the deployment.

Either way the flow is the same:

1. find the bundled `wireguard-go`;
2. check the answers before anything costs money, and discover the VPC, the
   public subnets, the route tables and the hosted zone for the hostname;
3. generate the WireGuard private key locally and keep it here;
4. deploy the stack with a change set, and wait for the gateway to publish its
   own public key to SSM;
5. register this workstation in the gateway's peer list and behind the load
   balancer;
6. write `/etc/wireguard/wg0.conf`, the private key and the sudoers rule — the
   only steps that need root;
7. install the app, and offer to open it at login;
8. raise the tunnel, ping the gateway, wait for the load balancer target to go
   healthy, and check that the public hostname returns 200.

It stops at the first step that fails and names it, rather than reporting success
on a half-built tunnel. Re-running resumes: existing keys and stacks are reused,
and a deploy with nothing to change is treated as success.

In the window, the privileged step is a single macOS authorisation dialog. In a
terminal, `sudo` prompts as usual.

### More than one workstation

A deployment can serve from several machines. The peer list lives in SSM at
`/<stack>/wireguard/peers` and the gateway reconciles against it once a minute,
so adding a workstation is one API call rather than a stack update — and it does
not interrupt the machines already serving.

```sh
wiregard-mini-vpn peers --stack xprem-onprem-vpn
wiregard-mini-vpn peers --stack xprem-onprem-vpn --remove 10.100.0.3
```

Run the installer on the second machine with `--client-ip 10.100.0.3`.

### Keeping the tunnel across a reboot

`--supervise`, or the checkbox in the setup window, installs a LaunchDaemon that
holds the tunnel at whatever state the menu bar last asked for. It is not "keep
the tunnel up": the switch is still the only thing that decides. The app writes
`up` or `down` to a file it owns, with no privileges at all, and root reconciles
towards it — including re-pinning a tunnel whose interface still exists but whose
peer has gone quiet, which is what a changed public address looks like from here.

## Updates

The app updates itself through [Sparkle](https://sparkle-project.org). Updates
are delivered as the same signed, notarized `.pkg` the first install uses —
`sparkle:installationType="package"` — because this product owns root-owned
files outside the app bundle, and swapping the bundle alone would leave the
helper, the supervisor and `wireguard-go` at the old version.

Cutting a release:

```sh
make sparkle-keys                     # once, ever: EdDSA keypair into the login Keychain
make pkg-notarized PROFILE=wiregard \
     APPCAST_FEED_URL=https://downloads.example.com/xpremvpn/appcast.xml \
     SPARKLE_PUBLIC_KEY=<the public half>
make appcast APPCAST_BASE_URL=https://downloads.example.com/xpremvpn
```

### Where the feed lives

`infra/cloudformation-updates.yaml` is the feed's own infrastructure — a private
S3 bucket published through one CloudFront distribution. It belongs to **your**
account, not a customer's: the VPN stack is deployed once per customer and pays
for itself there, while this one is deployed once and every copy of the app ever
shipped reads from it.

```sh
make feed-setup FEED_BUCKET=xpremvpn-downloads FEED_DOMAIN=downloads.example.com
```

It prints the two URLs a release needs. Deploy it in `us-east-1`: CloudFront
only reads ACM certificates from there.

CloudFront rather than API Gateway in front of S3, because API Gateway caps an
integration payload at 10 MB with no setting to raise it. The appcast is about a
kilobyte and would fit; the 30-plus MB release archive would not, so the archive
would need a second front door. CloudFront serves both from one hostname and
costs less per request.

The archives are a plain public download, and that is deliberate. Sparkle
refuses any archive whose EdDSA signature does not verify against the public key
compiled into the app, so the signature is the security boundary and the
transport is a convenience.

### Publishing

```sh
make release PROFILE=wiregard \
     APPCAST_FEED_URL=https://downloads.example.com/appcast.xml \
     SPARKLE_PUBLIC_KEY=<the public half> \
     APPCAST_BASE_URL=https://downloads.example.com/releases
```

That runs the checks, builds and notarizes the package, writes and signs the
feed, uploads both, invalidates the cached feed, and then reads the feed back
over its public URL — because everything before that step proves what was
uploaded, and only that step proves what will be served.

`make publish` does the upload half on its own. It refuses to overwrite an
archive for a version that is already published: two machines running different
software under one version number is a problem that surfaces months later. Raise
`VERSION`, or pass `--force` if it is genuinely the same build.

The archive is uploaded before the appcast, always. A feed naming an archive
that is not there yet is a feed every installed copy tries and fails to update
from.

`make appcast-validate` re-checks a feed without rewriting it — that the XML
parses, that the enclosure's length matches the file, that the advertised
version matches the package, and that the built app actually carries a feed URL
and a public key. That last check exists because the failure it catches is
silent: an app built without `APPCAST_FEED_URL` is a perfectly good app that
will never ask for an update, and nothing else notices.

Three things worth knowing:

- **`CFBundleVersion` is the release number, not the commit.** Sparkle compares
  that field to decide what is newer, and a git hash does not order. The commit
  is in `XpremBuildCommit`.
- **`sign_update --verify` reads the login Keychain and ignores
  `--ed-key-file`.** A release cut on a machine that holds the key is verified
  end to end; one signed from an exported key file — CI — is signed by Sparkle's
  own signer but cannot be re-verified in the same run, and `make appcast` says
  so rather than implying otherwise.
- **Back the private key up.** Losing it means no installed copy can ever be
  updated again, and every user has to be sent a package by hand.

An app built without `APPCAST_FEED_URL` simply has no updater, and says
"Built without an update feed" in its menu rather than failing quietly.

## Permissions

`docs/deploy-policy.json` is the least-privilege policy for the identity that
runs the installer. Attach it to a dedicated deploy user or role rather than
handing the installer an administrator key.

Day-to-day operation needs almost none of it: the running gateway uses its own
instance role, and adding or retiring a workstation needs only the SSM parameter
and target-registration statements.

### The sudoers rule

The menu bar toggles the tunnel through one `NOPASSWD` grant, scoped to two
exact command lines:

```
<user> ALL=(root) NOPASSWD: /Library/PrivilegedHelperTools/ca.maragato.xprem.vpn.helper tunnel up wg0, \
                            /Library/PrivilegedHelperTools/ca.maragato.xprem.vpn.helper tunnel down wg0
```

The path matters as much as the arguments. An earlier version of this rule
pointed at `/opt/homebrew/bin/wg-quick`; Homebrew owns that directory as the
logged-in user, mode 775, so the very account the rule named could replace the
file and become root without a password. `/Library/PrivilegedHelperTools` is
root-owned and is not somewhere a package manager takes ownership of.

The rule's remaining safety condition is that `/etc/wireguard/wg0.conf` stays
root-owned and mode 0600, because it names the key the helper loads and the peer
it trusts.

## Layout

| Path | What it is |
| --- | --- |
| `infra/cloudformation-xprem-onprem-vpn.yaml` | the AWS side: NLB, TLS listener, ACM certificate, gateway group, alarms |
| `cmd/wiregard-mini-vpn` | the installer, the uninstaller, the tunnel and the supervisor |
| `cmd/build-pkg` | builds, signs and notarizes the `.pkg` |
| `cmd/fetch-wireguard` | fetches and builds the pinned `wireguard-go` |
| `cmd/fetch-sparkle` | fetches the pinned Sparkle framework and its signing tools |
| `cmd/appcast` | builds and checks the Sparkle update feed |
| `cmd/publish` | deploys the feed's infrastructure and publishes a release to it |
| `infra/cloudformation-updates.yaml` | the vendor's S3 bucket and CloudFront distribution |
| `internal/awsops` | every AWS call, through the SDK |
| `internal/tunnel` | the WireGuard interface: keys, config, UAPI, routes |
| `internal/setup` | what to ask, what to write, what to verify |
| `menubar/` | the status bar app and its setup window |
| `docs/deploy-policy.json` | least-privilege IAM policy for the installer |

## Building

```sh
make check                            # vet, gofmt, tests, cfn-lint
make app                              # the universal .app bundle
make pkg                              # signs if the certificates are present
make pkg-notarized PROFILE=wiregard   # signs, notarizes, staples
```

Everything ships universal — `arm64` and `x86_64` — and the build refuses to
package a binary that is not, because a single-slice bundle installs cleanly and
then fails to launch on the other half of the Macs it reaches.

Signing needs two certificates, both created under Xcode › Settings › Accounts ›
Manage Certificates, and both requiring the Account Holder role:

| Certificate | Signs |
| --- | --- |
| Developer ID Application | `XpremVpn.app`, the engine and `wireguard-go` |
| Developer ID Installer | the `.pkg` |

An **Apple Distribution** certificate is not a substitute — that one is for the
App Store, and Gatekeeper rejects it for a direct download. Without these,
`make pkg` still produces a working package and says what is missing.

Notarization credentials are stored once:

```sh
xcrun notarytool store-credentials wiregard \
  --apple-id <apple-id> --team-id <team-id> --password <app-specific-password>
```

## Running cost

Roughly USD 26/month in the customer's account: NLB about 16 plus LCUs, a
`t4g.micro` gateway about 6, and an Elastic IP about 3.60. Alarms and the SNS
topic are only created when an address is given to notify.

## Licence

GPL-3.0-or-later; the full text is in `LICENSE`, and every source file carries
an SPDX header. Third-party components and their licences are listed in
`THIRD-PARTY-NOTICES.md` — all of them are GPLv3-compatible.

Two consequences worth knowing before distributing a build:

- **Anyone you give a binary to may have the source, and may pass both on.**
  That is the licence working as intended, not a loophole. If the business
  model depends on customers not being able to redistribute, GPLv3 is the wrong
  choice and this is the moment to change it.
- **The App Store is closed to this.** Apple's terms impose usage restrictions
  the GPL forbids, which is why GPL apps get pulled from it. Direct download
  with a Developer ID — what this repository builds — is unaffected.

## Known limits

- The gateway is one instance at a time. It is an Auto Scaling group of one
  across every available zone, and a replacement claims the Elastic IP, rewrites
  the VPN route and reads its WireGuard identity back out of SSM — so a failure
  is self-healing rather than permanent — but a replacement still takes a few
  minutes during which the hostname is dark.
- macOS only. There is no Windows or Linux client.
- The stack claims one hostname. Two deployments in one account need different
  `--domain` and `--stack` values; everything they own is namespaced by stack
  name, including the SSM parameters.
