# wiregard_mini_vpn

A small WireGuard tunnel that lets an AWS Network Load Balancer publish a
service running on a workstation, plus the macOS menu bar switch that raises and
drops that tunnel on demand.

It exists because `update.mobile.maragato.ca` is served by an
[xprem](https://github.com/MercureTechnologies/xprem) container running on a
laptop rather than on Fargate, and the laptop should only be reachable while
someone wants it to be.

```
client ──TLS──▶ NLB :443 ──▶ 10.100.0.2:3000
                              │
                      VPC route 10.100.0.0/24
                              ▼
                      gateway instance (EIP)
                              │  WireGuard
                              ▼
                      workstation 10.100.0.2 ──▶ xprem :3000
```

## Why not AWS Site-to-Site VPN

An `AWS::EC2::CustomerGateway` needs a fixed public IP address, because AWS
answers the IKE negotiation rather than starting it. The workstation sits behind
a residential NAT with a dynamic address. AWS does support a certificate-based
customer gateway with no address for exactly this case, but it requires an ACM
Private CA at roughly USD 400 per month, an order of magnitude more than
everything else here combined.

WireGuard inverts the direction. The workstation dials out to an Elastic IP, so
no inbound port forward is needed on the home router, and `PersistentKeepalive`
re-pins the tunnel after the ISP hands out a new address.

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
make install     # deploy and configure
make uninstall   # remove; the stack and the private key need an explicit yes
```

From the package: open `XpremVpn.pkg`. It installs the app and opens it, and the
first run is the setup window — AWS profile or access key, hostname, stack name,
then a live log of the deployment.

Either way the flow is the same:

1. check for `wg-quick`, installing it through Homebrew if missing;
2. generate the WireGuard private key locally and hand only the public half to
   CloudFormation;
3. deploy the stack with a change set, and wait for the gateway to publish its
   own public key to SSM;
4. write `/etc/wireguard/wg0.conf`, the private key, and the sudoers rule — the
   only steps that need root;
5. install the app, and offer to open it at login;
6. raise the tunnel, ping the gateway, wait for the load balancer target to go
   healthy, and check that the public hostname returns 200.

It stops at the first step that fails and names it, rather than reporting success
on a half-built tunnel. Re-running resumes: existing keys and stacks are reused,
and a deploy with nothing to change is treated as success.

In the window, the privileged step is a single macOS authorisation dialog
covering only those three file writes. In a terminal, `sudo` prompts as usual.

## Layout

| Path | What it is |
| --- | --- |
| `infra/cloudformation-xprem-onprem-vpn.yaml` | the AWS side: NLB, TLS listener, ACM certificate, gateway instance, tunnel route, Route53 alias |
| `cmd/wiregard-mini-vpn` | the installer and uninstaller |
| `cmd/build-pkg` | builds, signs and notarizes the `.pkg` |
| `internal/awsops` | every AWS call, through the SDK |
| `internal/setup` | what to ask, what to write, what to verify |
| `menubar/` | the status bar app and its setup window |

## Building the installer package

```sh
make pkg                              # signs if the certificates are present
make pkg-notarized PROFILE=wiregard   # signs, notarizes, staples
```

Signing needs two certificates, both created under Xcode › Settings › Accounts ›
Manage Certificates, and both requiring the Account Holder role:

| Certificate | Signs |
| --- | --- |
| Developer ID Application | `XpremVpn.app` and the engine binary inside it |
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

Roughly USD 26/month: NLB about 16 plus LCUs, a `t4g.micro` gateway about 6, and
an Elastic IP about 3.60.

## Known limits

- The gateway is a single instance in a single availability zone. If it stops,
  the hostname goes dark.
- The tunnel does not survive a reboot on the workstation; the menu bar app is a
  switch, not a supervisor.
- The stack claims `update.mobile.maragato.ca`, the same hostname as the Fargate
  stack in `maragato_xprem`. The two cannot be deployed together without changing
  `ServiceDomainName` on one of them.
- The app is built for `arm64` only.
