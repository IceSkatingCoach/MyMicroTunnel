# Changelog

Sparkle shows the entry for a release in the update dialog, so what is written
here is what a user reads when deciding whether to install. Write for them:
what changed and whether it matters, not which functions moved.

The format is one `## <version>` heading per release, newest first.
`make appcast` reads the section matching `VERSION` and embeds it in the feed.

## 1.0.3

- The menu bar can now remove Xprem VPN from this Mac. It explains what it does
  not touch — your private key, and the AWS stack, which keeps costing money
  until you delete it from a terminal.
- New `wiregard-mini-vpn diagnose` collects everything a support conversation
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
