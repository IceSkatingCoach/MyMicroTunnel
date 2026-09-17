# Xprem VPN menu bar app

A macOS status bar switch for the WireGuard tunnel created by
`infra/cloudformation-xprem-onprem-vpn.yaml`. The tunnel carries traffic from
the AWS Network Load Balancer behind `update.mobile.maragato.ca` to the xprem
container running on this machine, and it is meant to be up only while the
on-premises deployment should be reachable.

The icon sits at the right of the menu bar, near the clock. It is a padlock
shield: filled while the tunnel is up, outlined while it is down.

## Build and install

```sh
make app       # builds build/XpremVpn.app
make install   # copies it to /Applications
```

One source file and `swiftc`; no Xcode project and no dependencies. The bundle
is ad-hoc signed so macOS keeps a stable identity for it across rebuilds.

## Passwordless toggling

`wg-quick` must run as root. The app calls it through `sudo -n`, so without the
sudoers drop-in every toggle fails and the error appears in the menu instead of
a password prompt.

```sh
sudo install -m 0440 -o root -g wheel sudoers-xprem-vpn /etc/sudoers.d/xprem-vpn
sudo visudo -c -f /etc/sudoers.d/xprem-vpn
```

Read `sudoers-xprem-vpn` before installing it: it grants passwordless root for
two fixed command lines, and its safety depends on `/etc/wireguard/wg0.conf`
staying root-owned and mode 0600.

## Start at login

System Settings › General › Login Items › Open at Login › **+** ›
`/Applications/XpremVpn.app`. The app itself starts nothing: opening it at
login only puts the switch in the menu bar, it does not raise the tunnel.

## How connection state is detected

The app looks for the tunnel address `10.100.0.2` in `ifconfig` output every
three seconds. That needs no privileges, unlike `wg show`, and it tracks
tunnels raised or dropped from a terminal too.

Interface presence only proves `wg-quick` ran. **Test tunnel** pings the AWS
side of the tunnel at `10.100.0.1` and proves packets actually cross.
