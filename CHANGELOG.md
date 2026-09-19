# Changelog

Sparkle shows the entry for a release in the update dialog, so what is written
here is what a user reads when deciding whether to install. Write for them:
what changed and whether it matters, not which functions moved.

The format is one `## <version>` heading per release, newest first.
`make appcast` reads the section matching `VERSION` and embeds it in the feed.

## 1.1.0

- **The product is now called MyMicroTunnel.** The app, the command and the
  paths it installs are renamed with it: the command is `mymicrotunnel`, the app
  is `MyMicroTunnel.app`, and settings move to
  `~/Library/Application Support/MyMicroTunnel/profiles/`.
- Up to ten extra TCP ports can be published on the same hostname, forwarded
  as plain TCP alongside the HTTPS service — a database, an SSH daemon,
  anything. `--tcp-ports 5432,6379`. They are open to the internet, so put
  authentication on them.
- One Mac can now hold several deployments at once. Each VPN profile has its own
  stack, tunnel subnet, WireGuard interface, published ports and switch in the
  menu: `mymicrotunnel install --vpn-profile lab --vpn-cidr 10.110.0.0/24`.
- The tunnel subnet is yours to choose per profile, with `--vpn-cidr`, and two
  profiles that would collide are refused before anything is deployed.
- New: let the gateway sleep. With `--idle-timeout 30`, the EC2 instance is
  switched off after thirty minutes with no traffic through the load balancer,
  and comes back when you press Connect — about two minutes — or when the
  background service notices a tunnel that should be up has gone quiet. The
  hostname, the certificate and the address all survive; most of the bill does
  not.
- Waking runs through an IAM role the stack creates for that one purpose. It can
  ask this deployment's Auto Scaling group for an instance and nothing else, so
  the credential your Mac uses daily is not one that could delete the
  deployment.
- The hostname's DNS record is now written by the installer rather than by
  CloudFormation, so deploying onto a name that already exists in your zone
  repoints it instead of rolling the whole stack back. The zone itself is never
  created or touched. The hostname must have three labels — `updates.example.com`,
  not `example.com`, which is the apex your website usually sits on.
- A tunnel is refused, at install and every time it is raised, when its subnet
  overlaps a network this Mac is already on or another profile's subnet. The
  old behaviour was to come up and quietly take the LAN away: no printer, no
  router, no obvious cause.
- The default stack name is now `mymicrotunnel-<account-id>-<region>`, so a second
  deployment in an account no longer has to be named by hand to avoid
  overwriting the first.

## 1.0.3

- The menu bar can now remove MyMicroTunnel from this Mac. It explains what it does
  not touch — your private key, and the AWS stack, which keeps costing money
  until you delete it from a terminal.
- New `mymicrotunnel diagnose` collects everything a support conversation
  would otherwise ask for over three rounds of email: versions, tunnel state and
  handshake age, whether the published service answers on loopback *and* on the
  tunnel address, stack outputs, peer registry, target health, and the
  supervisor's last decisions. Safe to paste — the AWS account is masked and no
  secret is read.
- Installing no longer starts with pasting IAM JSON into a console. A
  launch-stack link creates the deploy identity in your own account.
- Fixed: a configuration file written by an older version was discarded whole
  rather than read partially, so the app silently fell back to compiled
  defaults after an update — the wrong interface at the wrong address. This
  would have affected every future update; it affects none now.

## 1.0.2

- Fixed: the release archive held the package one directory down from where
  Sparkle looks for it. Updates downloaded and verified correctly and then
  failed to install. 1.0.1 is affected and superseded by this release.

## 1.0.1

- Superseded by 1.0.2. Its update archive is malformed and will not install.

## 1.0.0

- First release.
- Publishes a service running on your Mac at a public HTTPS hostname, through an
  AWS Network Load Balancer and a WireGuard tunnel in your own AWS account.
- The menu bar switch raises and drops the tunnel; the hostname answers only
  while it is up.
- Carries its own WireGuard, so there is nothing to install first — no Homebrew,
  no AWS CLI, no Python.
- Optionally restores the tunnel to its last state after a reboot, and re-pins it
  when your home IP address changes.
- Several Macs can serve one hostname; adding one does not interrupt the others.
- The gateway is an Auto Scaling group of one, so a failed instance is replaced
  and keeps the same address and the same WireGuard identity.
