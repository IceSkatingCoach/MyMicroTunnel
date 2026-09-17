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

## Layout

| Path | What it is |
| --- | --- |
| `infra/cloudformation-xprem-onprem-vpn.yaml` | The AWS side: NLB, TLS listener, ACM certificate, gateway instance, tunnel route, Route53 alias |
| `menubar/` | The macOS status bar app that toggles the tunnel |

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
