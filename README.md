# Xprem VPN

Publish a service running on your Mac at a public HTTPS hostname, and turn it
off again when you are done.

An AWS Network Load Balancer terminates TLS and forwards to your workstation
over a WireGuard tunnel. A menu bar switch raises and drops that tunnel, so the
hostname answers only while you want it to.

```
client ──TLS──▶ NLB :443 ──▶ 10.100.0.2:3000
                              │
                      VPC route 10.100.0.0/24
                              ▼
                      gateway instance (Elastic IP)
                              │  WireGuard
                              ▼
                      your Mac 10.100.0.2 ──▶ your service :3000
```

Everything runs in **your** AWS account. Nothing is hosted for you, and no
traffic passes through anyone else.

---

# Installing

## Before you start

You need five things. The install stops and names whichever one is missing
rather than half-finishing, but it is quicker to have them ready.

| | |
| --- | --- |
| A Mac | macOS 13 or later, Apple silicon or Intel |
| An administrator account | one password prompt, for three files |
| An AWS account | the resources cost roughly **USD 26/month** |
| A domain in Route53 | in that same account, as a *public* hosted zone |
| A service to publish | running on your Mac, on a known port |

You do **not** need Homebrew, the AWS CLI, Node, Python, or WireGuard. The
installer carries everything it uses.

### Your service must listen on all interfaces

This is the one that catches people. The load balancer reaches your Mac at its
tunnel address, `10.100.0.2` — not at `127.0.0.1`. A service bound to loopback
is invisible to it, and the only symptom is a health check that never passes.

With Docker, the published port must not be pinned to loopback:

```sh
docker run -p 3000:3000 ...            # reachable over the tunnel
docker run -p 127.0.0.1:3000:3000 ...  # not reachable, health checks fail
```

Check it the way the load balancer will, once the tunnel is up:

```sh
curl http://10.100.0.2:3000/hc
```

Your service also needs a path that returns **200** for health checks. `/hc` is
the default; you can set another during setup.

---

## Step 1 — Create AWS credentials for the installer

The installer deploys into your account, so it needs a key. Give it a dedicated
one with only the permissions it uses, rather than an administrator key.

1. AWS console → **IAM** → **Policies** → **Create policy**.
2. Choose the **JSON** tab and paste [`docs/deploy-policy.json`](docs/deploy-policy.json).
3. Name it `XpremVpnDeploy` and create it.
4. **Users** → **Create user**, name it `xprem-vpn-deploy`, attach that policy.
5. Open the user → **Security credentials** → **Create access key** →
   **Command Line Interface**. Copy the access key ID and the secret.

You will paste that secret once and not need it again.

> Already have the AWS CLI configured with a profile that can deploy? Skip this
> step — the setup window offers your existing profiles instead.

## Step 2 — Check your hosted zone

**Route53** → **Hosted zones**. You need a **public** zone for the domain you
intend to serve from: `example.com` if the hostname will be
`updates.example.com`.

The installer finds the zone itself. It only has to exist, and be public rather
than private.

## Step 3 — Install the app

Open `XpremVpn.pkg` and follow the installer. It places:

```
/Applications/XpremVpn.app                                   the menu bar app
/usr/local/bin/wiregard-mini-vpn                             the same tool, for terminals
/Library/PrivilegedHelperTools/ca.maragato.xprem.vpn.helper  the part that needs root
/usr/local/lib/wiregard-mini-vpn/wireguard-go                the tunnel itself
```

The app opens by itself when the installer finishes.

## Step 4 — Fill in the setup window

| Field | What to put |
| --- | --- |
| **AWS credentials** | An existing profile, or *Enter an access key* and the two values from step 1 |
| **Region** | Leave empty to use the region your profile already names |
| **Stack name** | `xprem-onprem-vpn` unless you are deploying a second one |
| **Public hostname** | The name to serve, e.g. `updates.example.com` |
| **Local service port** | The port your service listens on |
| **Health check path** | A path that returns 200. Default `/hc` |
| **Notify on failure** | Optional. An address to alarm when the gateway dies |
| **Restore the tunnel after a reboot** | Tick if the hostname should come back on its own |

Press **Install**. The window shows each step as it happens.

## Step 5 — Approve the one password prompt

Partway through, macOS asks for your administrator password. It covers exactly
three things:

- `/etc/wireguard/wg0.conf` — the tunnel's configuration
- `/etc/wireguard/client.key` — your private key, which never leaves the Mac
- `/etc/sudoers.d/xprem-vpn` — permission for the menu bar to move the tunnel
  without asking again

If you ticked *Restore the tunnel after a reboot*, it also installs the
background service that does that.

## Step 6 — Wait for it to prove itself

The last stage is not a summary; it is a series of checks, in the order traffic
travels:

1. the tunnel comes up
2. the gateway answers a ping
3. the load balancer reports your Mac as healthy
4. `https://your-hostname/hc` returns 200

If any fails it says which, and stops. A clean run means the hostname is live
right now.

Roughly ten minutes end to end, most of it AWS building the load balancer.

---

# Using it

The padlock in the menu bar is the switch. Filled is connected; outline is not.

| | |
| --- | --- |
| **Connect / Disconnect** | Raise or drop the tunnel. Disconnected means the hostname stops answering |
| **Test tunnel** | Pings the gateway. Proves packets cross, not just that an interface exists |
| **Open health check** | Opens your hostname in a browser |
| **Setup…** | Re-run the deployment, or change a setting |
| **Check for Updates…** | Fetches a new version if there is one |

If you turned on *Restore the tunnel after a reboot*, the switch is still the
only thing that decides. The background service remembers your last choice and
puts it back after a restart or a sleep — including re-pinning the tunnel when
your home IP address changes, which otherwise leaves an interface that exists
but carries nothing.

## Adding a second Mac

A deployment can serve from several machines. Run the installer on the second
one with a different tunnel address:

```sh
wiregard-mini-vpn install --domain updates.example.com --client-ip 10.100.0.3
```

Adding a machine does not interrupt the ones already serving.

```sh
wiregard-mini-vpn peers                        # who is registered
wiregard-mini-vpn peers --remove 10.100.0.3    # retire one
```

## Checking on it

```sh
wiregard-mini-vpn status                       # is the tunnel up
sudo wiregard-mini-vpn tunnel status wg0       # and is anything crossing it
```

The second needs `sudo` because the handshake is only readable by root.

---

# When something is wrong

**The hostname does not answer, but the tunnel is up.**
Almost always the service is bound to loopback. If `curl http://10.100.0.2:3000/hc`
is refused while `127.0.0.1` works, rebind the service to all interfaces.

**The tunnel connects but nothing crosses it.**
`sudo wiregard-mini-vpn tunnel status wg0`. A peer with `last handshake never`
means the gateway has not accepted this Mac. Re-run setup.

**"wg-quick up failed", or anything mentioning bash.**
An old version is still installed. This product does not use `wg-quick`.
Re-install the package.

**The load balancer says unhealthy.**
The health check path must return 200 over plain HTTP on your service's port.
Test it exactly as the load balancer does: `curl -i http://10.100.0.2:3000/hc`.

**A deploy failed partway.**
Re-run it. Existing keys and stacks are reused, and a deploy with nothing to
change is treated as success.

## Removing it

```sh
wiregard-mini-vpn uninstall
```

Removes the app, the tunnel, the sudoers rule and the background service, and
takes this Mac out of the deployment so the load balancer stops sending it
traffic. Your private key and the AWS stack are only touched if you say yes to
each — deleting the stack takes the hostname down for every machine.

---

# What it costs

About **USD 26/month** in your account:

| | |
| --- | --- |
| Network Load Balancer | ~16, plus capacity units |
| `t4g.micro` gateway | ~6 |
| Elastic IP | ~3.60 |

Alarms and their notification topic are only created if you give an address to
notify.

---

# How it works

## Why WireGuard and not AWS Site-to-Site VPN

`AWS::EC2::CustomerGateway` needs a fixed public IP, because AWS answers the IKE
negotiation rather than starting it. A workstation behind a residential NAT does
not have one. AWS does support a certificate-based customer gateway with no
address for this case, but it needs an ACM Private CA at roughly USD 400/month —
more than everything else here combined.

WireGuard inverts the direction. Your Mac dials out to an Elastic IP, so no
inbound port forward is needed on your router, and `PersistentKeepalive` re-pins
the tunnel when your ISP changes your address.

## Nothing to install first

macOS has no in-kernel WireGuard, so the data plane runs in userspace: that is
`wireguard-go`, MIT-licensed, built from a pinned and checksummed release and
shipped inside the package.

`wg(8)` and `wg-quick(8)` are **not** shipped and not needed. `wg-quick` on macOS
is a bash script that needs bash 4, which macOS has not shipped since 2007 —
which is why Homebrew's formula pulls in its own. Everything those tools did is
in `internal/tunnel`, which speaks wireguard-go's
[UAPI](https://www.wireguard.com/xplatform/) directly. The config written to
`/etc/wireguard/wg0.conf` is still wg-quick's format, so those tools can drive
the same tunnel if you happen to have them.

## The gateway repairs itself

It is an Auto Scaling group of one. A replacement claims the Elastic IP,
rewrites the VPN route, turns off its own source/destination check and reads its
WireGuard identity back out of SSM — so a failed instance is replaced rather
than mourned, and every Mac's config keeps pointing somewhere real. A
replacement still takes a few minutes, during which the hostname is dark.

## The privileged part is small

The menu bar moves the tunnel through one `NOPASSWD` rule, scoped to two exact
command lines:

```
<user> ALL=(root) NOPASSWD: /Library/PrivilegedHelperTools/ca.maragato.xprem.vpn.helper tunnel up wg0, \
                            /Library/PrivilegedHelperTools/ca.maragato.xprem.vpn.helper tunnel down wg0
```

The path matters as much as the arguments. An earlier version pointed at
`/opt/homebrew/bin/wg-quick`; Homebrew owns that directory as the logged-in user,
mode 775, so the very account the rule named could replace the file and become
root. `/Library/PrivilegedHelperTools` is root-owned and is not somewhere a
package manager takes ownership of.

The rule's remaining safety condition is that `/etc/wireguard/wg0.conf` stays
root-owned and mode 0600, because it names the key the helper loads.

---

# For maintainers

## Layout

| Path | What it is |
| --- | --- |
| `infra/cloudformation-xprem-onprem-vpn.yaml` | the customer's stack: NLB, TLS, gateway group, alarms |
| `infra/cloudformation-updates.yaml` | the vendor's stack: S3 and CloudFront for the update feed |
| `infra/cloudformation-site.yaml` | the vendor's stack: the product website |
| `cmd/wiregard-mini-vpn` | installer, uninstaller, tunnel, supervisor |
| `cmd/build-pkg` | builds, signs and notarizes the `.pkg` |
| `cmd/fetch-wireguard`, `cmd/fetch-sparkle` | pinned third-party binaries |
| `cmd/appcast`, `cmd/publish` | builds, checks and publishes a release |
| `internal/awsops` | every AWS call, through the SDK |
| `internal/tunnel` | keys, config, UAPI, routes |
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
then fails to launch on half the Macs it reaches.

Signing needs two certificates, both created under Xcode › Settings › Accounts ›
Manage Certificates, and both requiring the Account Holder role:

| Certificate | Signs |
| --- | --- |
| Developer ID Application | the app, the engine, `wireguard-go`, Sparkle |
| Developer ID Installer | the `.pkg` |

An **Apple Distribution** certificate is not a substitute — that is for the App
Store, and Gatekeeper rejects it for a direct download.

## Releasing

```sh
make sparkle-keys                     # once, ever. Back the private half up
make feed-setup FEED_BUCKET=<name> FEED_DOMAIN=<hostname>   # once
make release PROFILE=wiregard \
     APPCAST_FEED_URL=https://<hostname>/appcast.xml \
     SPARKLE_PUBLIC_KEY=<the public half> \
     APPCAST_BASE_URL=https://<hostname>/releases
```

`make release` runs the checks, notarizes, writes and signs the feed, uploads
both, invalidates the cache, and then reads the feed back over its public URL —
because everything before that proves what was uploaded, and only that proves
what will be served.

Two things worth knowing:

- **`CFBundleVersion` is the release number, not the commit.** Sparkle compares
  it, and a git hash does not order. The commit is in `XpremBuildCommit`.
- **Losing the Sparkle private key is unrecoverable.** No installed copy could
  ever be updated again. Export it and keep it somewhere durable:
  `./third_party/sparkle/bin/generate_keys -x sparkle-private-key.txt`

## Licence

GPL-3.0-or-later; see [`LICENSE`](LICENSE). Every source file carries an SPDX
header. Third-party components are listed in
[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md); all are GPLv3-compatible.

Distributing a build means the recipient may have the source and may pass both
on. If the business depends on customers not redistributing, GPLv3 is the wrong
licence and that decision should be revisited before the first sale. The App
Store is closed to GPL software; direct download with a Developer ID, which is
what this builds, is unaffected.

## Known limits

- The gateway is one instance at a time. It repairs itself, but a replacement
  takes a few minutes during which the hostname is dark.
- macOS only. There is no Windows or Linux client.
- One hostname per stack. A second deployment needs its own `--domain` and
  `--stack`; everything it owns is namespaced by stack name.

## Contributing

Patches welcome — <xpremvpn@maragato.ca>.

`make check` is the gate: `go vet`, `gofmt`, the tests, and `cfn-lint` over both
templates. CI runs the same plus a universal-slice check and `govulncheck`.
