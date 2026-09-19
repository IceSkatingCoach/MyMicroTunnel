# MyMicroTunnel

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

**[Create the deploy identity](https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review?templateURL=https%3A%2F%2Fmymicrotunnel-site-985658740042.s3.amazonaws.com%2Flaunch%2Fdeploy-role.yaml&stackName=mymicrotunnel-deploy-role)** — one click, in whichever
AWS account you are signed into. It creates an IAM user holding only the
permissions the installer uses.

Then open the stack's **Outputs** tab and follow `CreateAccessKeyUrl` →
**Create access key** → **Command Line Interface**. Copy the two values.

The template deliberately does not create the key for you. Stack outputs are
stored by CloudFormation and readable by anyone who can describe the stack,
forever; creating it yourself means the secret is shown once and stored nowhere.

<details>
<summary>Or do it by hand</summary>

1. AWS console → **IAM** → **Policies** → **Create policy**.
2. Choose the **JSON** tab and paste [`docs/deploy-policy.json`](docs/deploy-policy.json).
3. Name it `MyMicroTunnelDeploy` and create it.
4. **Users** → **Create user**, name it `mymicrotunnel-deploy`, attach that policy.
5. Open the user → **Security credentials** → **Create access key** →
   **Command Line Interface**.

A test keeps that JSON and the template above in step with each other.
</details>

> Already have the AWS CLI configured with a profile that can deploy? Skip this
> step — the setup window offers your existing profiles instead.

## Step 2 — Check your hosted zone

**Route53** → **Hosted zones**. You need a **public** zone for the domain you
intend to serve from: `example.com` if the hostname will be
`updates.example.com`.

The installer finds the zone itself. It only has to exist, and be public rather
than private — this never creates or deletes a zone, because yours predates
this deployment and usually carries your mail.

The hostname you serve needs three labels: a host part, the domain and the
top-level domain. `updates.example.com`, not `example.com` — the bare domain is
the zone apex, which is normally your website.

If the name already exists in the zone, it is repointed rather than refused.

## Step 3 — Install the app

Open `MyMicroTunnel.pkg` and follow the installer. It places:

```
/Applications/MyMicroTunnel.app                                   the menu bar app
/usr/local/bin/mymicrotunnel                             the same tool, for terminals
/Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper  the part that needs root
/usr/local/lib/mymicrotunnel/wireguard-go                the tunnel itself
```

The app opens by itself when the installer finishes.

## Step 4 — Fill in the setup window

| Field | What to put |
| --- | --- |
| **AWS credentials** | An existing profile, or *Enter an access key* and the two values from step 1 |
| **Region** | Leave empty to use the region your profile already names |
| **VPN profile** | `default`, unless this is a second deployment on the same Mac |
| **Stack name** | Leave empty. It becomes `mymicrotunnel-<account-id>-<region>` |
| **Public hostname** | The name to serve, three labels: `updates.example.com` |
| **Service port** | `3000` — or `3000:8443` to publish it on a port other than 443 |
| **Also publish** | Optional. Up to ten more, each `local:published`: `5432, 3000:8080` |
| **Health check path** | A path that returns 200. Default `/hc` |
| **Tunnel subnet** | The private range the tunnel uses. Default `10.100.0.0/24` |
| **Idle timeout (min)** | Optional. Switch the gateway off after this many quiet minutes |
| **Notify on failure** | Optional. An address to alarm when the gateway dies |
| **Restore the tunnel after a reboot** | Tick if the hostname should come back on its own |

Press **Install**. The window shows each step as it happens.

## Step 5 — Approve the one password prompt

Partway through, macOS asks for your administrator password. It covers exactly
three things:

- `/etc/wireguard/wg0.conf` — the tunnel's configuration
- `/etc/wireguard/client.key` — your private key, which never leaves the Mac
- `/etc/sudoers.d/mymicrotunnel` — permission for the menu bar to move the tunnel
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
| **Wake gateway** | Only on a deployment with an idle timeout. Asks AWS for the gateway back |
| **Open health check** | Opens your hostname in a browser |
| **Setup…** | Re-run the deployment, or change a setting |
| **Check for Updates…** | Fetches a new version if there is one |

With more than one VPN profile installed, each one gets its own block in the
menu with its own switch. The padlock is filled when any of them is up.

If you turned on *Restore the tunnel after a reboot*, the switch is still the
only thing that decides. The background service remembers your last choice and
puts it back after a restart or a sleep — including re-pinning the tunnel when
your home IP address changes, which otherwise leaves an interface that exists
but carries nothing.

## Publishing more than one port

The hostname answers HTTPS on 443 and forwards to your service port, with TLS
terminated at the load balancer. Up to ten further ports can be published as
plain TCP, on the same hostname, forwarded untouched:

```sh
mymicrotunnel install --domain updates.example.com --port 3000 --tcp-ports 5432,3000:8080
```

Every port is written `local:published`. A bare number publishes the port under
its own name; `3000:8080` reaches port 3000 on this Mac and answers as port 8080
on the hostname. `--port 3000:8443` moves the HTTPS service off 443.

`updates.example.com:5432` then reaches port 5432 on this Mac. These are not
TLS-terminated and are not health-checked with HTTP — the load balancer only
checks that the port accepts a connection — because what crosses them is
whatever protocol you are speaking. **Anything you publish this way is open to
the internet**: put authentication on it, or do not publish it.

Removing a port is the same command without it: the listener and its target
group are torn down.

## Letting the gateway sleep

The EC2 instance is most of the bill, and a deployment that is used during
office hours pays for it around the clock. With an idle timeout, it does not:

```sh
mymicrotunnel install --domain updates.example.com --idle-timeout 30
```

After thirty minutes with no connection crossing the load balancer, a CloudWatch
alarm scales the gateway to zero and the instance goes away. The hostname, the
certificate, the Elastic IP and your peer registration all stay; only the
machine in the middle stops existing, which is what it costs to run.

The first connection afterwards has to bring it back. The menu bar does that for
you when you press **Connect**, and the background service does it when a tunnel
that should be up has gone quiet. By hand:

```sh
mymicrotunnel wake                 # and wait for it
mymicrotunnel wake --wait 0        # just ask, do not wait
```

Waking takes about two minutes, because the gateway boots from scratch: it
re-claims the Elastic IP, re-reads its WireGuard identity from SSM, and applies
the peer list. This is deliberately not done with a stopped instance, which
would come back faster and keep charging for its disk.

The stack creates an IAM role for exactly this and nothing else. It may set the
desired capacity of this one Auto Scaling group to one, and read enough to know
whether that worked. It cannot change the group, launch anything of its own, or
touch any other deployment — so the credential your Mac uses several times a day
is not the one that could delete the load balancer.

## Running more than one deployment from one Mac

A VPN profile is one deployment as this Mac sees it: its own stack, its own
tunnel subnet, its own WireGuard interface, its own published ports, its own
switch in the menu.

```sh
mymicrotunnel install --vpn-profile work \
  --domain updates.example.com --vpn-cidr 10.100.0.0/24

mymicrotunnel install --vpn-profile lab \
  --domain lab.example.com --vpn-cidr 10.110.0.0/24 --tcp-ports 5432
```

The second one gets `wg1` without being asked, and refuses to install if it
would collide with the first on an interface or a tunnel address. Give each
profile its own `--vpn-cidr` and that cannot happen.

```sh
mymicrotunnel profile list          # what this Mac holds
mymicrotunnel profile show lab      # one of them in full
mymicrotunnel status --vpn-profile lab
mymicrotunnel uninstall --vpn-profile lab
```

Every command that acts on a deployment takes `--vpn-profile`, and reads the
stack, the AWS profile and the region back from what the install recorded, so
you do not have to repeat them.

## Adding a second Mac

A deployment can serve from several machines. Run the installer on the second
one with a different tunnel address:

```sh
mymicrotunnel install --domain updates.example.com --client-ip 10.100.0.3
```

Adding a machine does not interrupt the ones already serving.

```sh
mymicrotunnel peers                        # who is registered
mymicrotunnel peers --remove 10.100.0.3    # retire one
```

## Checking on it

```sh
mymicrotunnel status                       # is the tunnel up
sudo mymicrotunnel tunnel status wg0       # and is anything crossing it
```

The second needs `sudo` because the handshake is only readable by root.

---

# When something is wrong

**The hostname does not answer, but the tunnel is up.**
Almost always the service is bound to loopback. If `curl http://10.100.0.2:3000/hc`
is refused while `127.0.0.1` works, rebind the service to all interfaces.

**The tunnel connects but nothing crosses it.**
`sudo mymicrotunnel tunnel status wg0`. A peer with `last handshake never`
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
mymicrotunnel uninstall
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
<user> ALL=(root) NOPASSWD: /Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper tunnel up wg0, \
                            /Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper tunnel down wg0
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
| `infra/cloudformation-microtunnel.yaml` | the customer's stack: NLB, TLS, gateway group, alarms |
| `infra/cloudformation-updates.yaml` | the vendor's stack: S3 and CloudFront for the update feed |
| `infra/cloudformation-site.yaml` | the vendor's stack: the product website |
| `infra/cloudformation-deploy-role.yaml` | the customer's one-click IAM identity for installing |
| `cmd/mymicrotunnel` | installer, uninstaller, tunnel, supervisor |
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
make pkg-notarized PROFILE=microtunnel   # signs, notarizes, staples
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
make release PROFILE=microtunnel \
     APPCAST_FEED_URL=https://<hostname>/appcast.xml \
     SPARKLE_PUBLIC_KEY=<the public half> \
     APPCAST_BASE_URL=https://<hostname>/releases
```

`make release` runs the checks, notarizes, writes and signs the feed, uploads
both, invalidates the cache, and then reads the feed back over its public URL —
because everything before that proves what was uploaded, and only that proves
what will be served.

Release notes come from `CHANGELOG.md` — the section matching `VERSION` — and
are shown in Sparkle's update dialog. A release with no section ships an update
that says nothing about itself, and `make appcast` warns when that is about to
happen.

## Rolling a release back

```sh
make rollback ROLLBACK_TO=1.0.2 APPCAST_BASE_URL=https://<hostname>/releases
```

Fetches that version's archive from the URL the feed advertises — proving it is
still served — signs it, and republishes the feed pointing at it.

Sparkle does not downgrade, so anyone already on the bad version stays there.
Rolling back stops it reaching anybody else; getting those users off it needs a
higher version number, not a lower one.

Two things worth knowing:

- **`CFBundleVersion` is the release number, not the commit.** Sparkle compares
  it, and a git hash does not order. The commit is in `MyMicroTunnelBuildCommit`.
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

Patches welcome — <mymicrotunnel@maragato.ca>.

`make check` is the gate: `go vet`, `gofmt`, the tests, and `cfn-lint` over both
templates. CI runs the same plus a universal-slice check and `govulncheck`.
