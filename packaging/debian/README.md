# MyMicroTunnel for Debian 13

Publishes a service running on this machine at a public HTTPS hostname, through
an AWS Network Load Balancer and a WireGuard tunnel. It works as it does on
macOS, but without a menu bar: you move the tunnel from the command line, and a
systemd unit keeps it in the state you last chose across reboots.

## Install

```sh
tar xzf mymicrotunnel-debian13-<version>.tar.gz
cd mymicrotunnel-debian13-<version>
sudo ./install.sh
```

This installs `wireguard-tools`, `iproute2` and `sudo`, and puts the binary at
`/usr/libexec/mymicrotunnel/mymicrotunnel`, with a link to it at
`/usr/local/bin/mymicrotunnel`.

## Deploy

Run this as the user who owns the AWS credentials, not as root. It asks for
`sudo` once, to write the files that need root.

```sh
mymicrotunnel install --domain app.example.com --port 3000 --supervise
```

Your service must listen on `0.0.0.0` or on the tunnel address (`10.100.0.2` by
default), not only on `127.0.0.1`.

## Use

```sh
sudo /usr/libexec/mymicrotunnel/mymicrotunnel tunnel up wg0
sudo /usr/libexec/mymicrotunnel/mymicrotunnel tunnel down wg0
mymicrotunnel status
mymicrotunnel diagnose
journalctl -u mymicrotunnel-supervisor -f
```

The sudoers rule allows these two exact commands without a password, and only
through the root-owned path shown above.

## Differences from macOS

| | macOS | Debian |
| --- | --- | --- |
| WireGuard | bundled `wireguard-go` on a utun device | kernel module, driven by `wg` and `ip` |
| Keeping the tunnel up | LaunchDaemon | `mymicrotunnel-supervisor.service` |
| Profiles | `~/Library/Application Support/MyMicroTunnel` | `~/.config/mymicrotunnel` |
| Application AWS key | login Keychain | `~/.config/mymicrotunnel/aws/<account>`, mode 0600 |
| Switch | menu bar | `tunnel up` and `tunnel down` |

## Remove

```sh
mymicrotunnel uninstall --vpn-profile default --delete-stack
sudo ./install.sh --uninstall
```
