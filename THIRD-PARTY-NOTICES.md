# Third-party notices

MyMicroTunnel itself is GPL-3.0-or-later; see `LICENSE`. This file covers
the third-party code it ships and links, all of which is under licences
compatible with that.

## Shipped as binaries

### wireguard-go

- Version: `0.0.20230223`, from <https://git.zx2c4.com/wireguard-go>
- Licence: MIT
- Copyright: © 2017–2023 WireGuard LLC
- Installed at `/usr/local/lib/mymicrotunnel/wireguard-go` and inside
  `MyMicroTunnel.app/Contents/Resources/`
- Full licence text: `third_party/LICENSE.wireguard-go`, produced by
  `make wireguard`

macOS has no in-kernel WireGuard, so the data plane has to run in userspace.
This is that data plane, and it is the only part of WireGuard this product
ships.

**Modification.** The binary is built from the unmodified release tarball, whose
SHA-256 is pinned in `cmd/fetch-wireguard/main.go`, with three dependencies
upgraded so that it links against a current Go toolchain — the released pins
fail with `link: golang.org/x/net/internal/socket: invalid reference to
syscall.recvmsg`. The upgraded versions are listed in the same file. No source
file of wireguard-go itself is changed.

### Sparkle

- Version `2.10.0`, from <https://github.com/sparkle-project/Sparkle>
- Licence: MIT, with a BSD-licensed portion (see `third_party/sparkle/LICENSE.sparkle`)
- Embedded as `MyMicroTunnel.app/Contents/Frameworks/Sparkle.framework`, unmodified
- Fetched and checksummed by `make sparkle`

### What is deliberately not shipped

`wg(8)` and `wg-quick(8)` from `wireguard-tools` are **not** included, and are
not required.

- `wg-quick` on macOS is a bash script that declares associative arrays at the
  top level, so it needs bash 4. macOS has shipped bash 3.2 since 2007 for
  licensing reasons, which is why Homebrew's formula pulls in a newer bash.
- `wireguard-tools` is GPLv2-**only**, and this product is GPLv3. Those two are
  not compatible: GPLv2-only code cannot be combined into a GPLv3 work. Shipping
  `wg(8)` as a separate unmodified executable would be mere aggregation and
  therefore allowed, but it is moot — nothing here needs it.

Everything those two tools did here — generating keys, configuring the
interface, reading its state, adding routes — is done by `internal/tunnel`,
which talks to wireguard-go over its documented UAPI socket
(<https://www.wireguard.com/xplatform/>). The configuration file it writes is
still wg-quick's format, so a machine that does happen to have the Homebrew
tools installed can be used to inspect or drive the same tunnel.

## Linked into the binary

| Module | Licence |
| --- | --- |
| `github.com/aws/aws-sdk-go-v2` and its service clients | Apache-2.0 |
| `github.com/aws/smithy-go` | Apache-2.0 |
| `golang.org/x/crypto` | BSD-3-Clause |
| `golang.org/x/sys` | BSD-3-Clause |
| `golang.org/x/term` | BSD-3-Clause |

Every one of these is compatible with GPLv3. Apache-2.0 is worth a note: it is
compatible with GPLv3 and **not** with GPLv2, so GPLv3 is the only version of
the GPL this product could have been released under while linking the AWS SDK.

`go list -m all` prints the exact versions for any given build, and
`mymicrotunnel version` prints the commit those versions were resolved at.
