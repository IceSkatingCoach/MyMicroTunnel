# wiregard_mini_vpn

A small WireGuard tunnel that lets an AWS Network Load Balancer publish a
service running on a workstation, plus the macOS menu bar switch that raises
and drops that tunnel on demand.

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
answers the IKE negotiation rather than starting it. The workstation sits
behind a residential NAT with a dynamic address. AWS does support a
certificate-based customer gateway with no address for exactly this case, but
it requires an ACM Private CA at roughly USD 400 per month, an order of
magnitude more than everything else here combined.

WireGuard inverts the direction. The workstation dials out to an Elastic IP, so
no inbound port forward is needed on the home router, and `PersistentKeepalive`
re-pins the tunnel after the ISP hands out a new address.

## Install

```sh
make install
```

One command from a clean machine. It asks for an AWS profile — or an access key
and secret, which it stores as a new profile through the AWS CLI — then:

1. checks for the AWS CLI, `wg-quick` and `swiftc`, installing WireGuard through
   Homebrew if it is missing;
2. generates the WireGuard private key locally and hands only the public half to
   CloudFormation;
3. deploys the stack and waits for the gateway to publish its own public key;
4. writes `/etc/wireguard/wg0.conf` and the sudoers rule, both as root, asking
   for your password;
5. builds the menu bar app, installs it into `/Applications`, and offers to open
   it at login;
6. raises the tunnel, pings the gateway, waits for the load balancer target to
   go healthy, and finally checks that the public hostname returns 200.

It stops at the first step that fails and says which one, rather than reporting
success on a half-built tunnel. Re-running it resumes: existing keys, stacks and
files are reused instead of duplicated.

Prerequisites it will not install for you: the AWS CLI (`brew install awscli`)
and the Xcode command line tools (`xcode-select --install`). The AWS credentials
need permission to create the stack's resources — EC2, ELBv2, ACM, Route53, SSM
and IAM.

```sh
make uninstall
```

Removes the app, the login item, the sudoers rule and the local config without
asking. The private key and the CloudFormation stack are only removed if you say
yes to each.

## Layout

| Path | What it is |
| --- | --- |
| `infra/cloudformation-xprem-onprem-vpn.yaml` | The AWS side: NLB, TLS listener, ACM certificate, gateway instance, tunnel route, Route53 alias |
| `menubar/` | The macOS status bar app that toggles the tunnel |
| `installer/` | `install.ts` and `uninstall.ts`, the one-command setup and teardown |

Each has its own notes: the CloudFormation template carries its deploy order
and parameters in a header comment, and `menubar/README.md` covers building,
installing, and the sudoers drop-in the app depends on.

## Running cost

Roughly USD 26/month: NLB about 16 plus LCUs, a `t4g.micro` gateway about 6,
and an Elastic IP about 3.60.

## Known limits

- The gateway is a single instance in a single availability zone. If it stops,
  the hostname goes dark.
- The tunnel does not survive a reboot on the workstation; the menu bar app is
  a switch, not a supervisor.
- The stack claims `update.mobile.maragato.ca`, the same hostname as the
  Fargate stack in `maragato_xprem`. The two cannot be deployed together
  without changing `ServiceDomainName` on one of them.
