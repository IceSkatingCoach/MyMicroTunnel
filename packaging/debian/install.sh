#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Installs MyMicroTunnel on Debian 13 (trixie).
#
#   sudo ./install.sh              install the command and its dependencies
#   sudo ./install.sh --uninstall  remove the command; profiles and AWS stacks are kept
#
# It only installs the tool. Deploying a tunnel is a separate step, run as the
# user who owns the AWS credentials, not as root:
#
#   mymicrotunnel install --domain app.example.com --port 3000 --supervise
#
# Re-running is safe: it replaces the binary in place.

set -euo pipefail

HELPER=/usr/libexec/mymicrotunnel/mymicrotunnel
COMMAND=/usr/local/bin/mymicrotunnel
DOC_DIR=/usr/share/doc/mymicrotunnel

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
FORCE=0
UNINSTALL=0

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '4,14p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  cat <<'EOF'
Options:
  --force       skip the Debian 13 check
  --uninstall   remove the command, helper and supervisor unit
EOF
}

while [[ $# -gt 0 ]]; do
  case $1 in
    --force)     FORCE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           usage >&2; die "unknown option: $1" ;;
  esac
done

[[ $EUID -eq 0 ]] || die "run as root: sudo $0"

if [[ $UNINSTALL -eq 1 ]]; then
  # Tunnels first: once the helper is gone nothing can take them down cleanly.
  # The sudoers rule names every interface this product owns.
  if [[ -f /etc/sudoers.d/mymicrotunnel && -x $HELPER ]]; then
    for name in $(grep -o 'tunnel down [a-z0-9]*' /etc/sudoers.d/mymicrotunnel | awk '{print $3}' | sort -u); do
      "$HELPER" tunnel down "$name" >/dev/null 2>&1 || true
    done
  fi
  systemctl disable --now mymicrotunnel-supervisor.service 2>/dev/null || true
  rm -f /etc/systemd/system/mymicrotunnel-supervisor.service /etc/sudoers.d/mymicrotunnel
  systemctl daemon-reload 2>/dev/null || true
  rm -f "$COMMAND"
  rm -rf "$(dirname "$HELPER")" "$DOC_DIR"
  log "removed the command, helper, supervisor and sudoers rule"
  echo "Kept: /etc/wireguard (keys), ~/.config/mymicrotunnel (profiles), and every AWS stack."
  echo "To delete a deployment, run \`mymicrotunnel uninstall --vpn-profile <name>\` before this."
  exit 0
fi

# --- preflight ---------------------------------------------------------------

. /etc/os-release
if [[ ${ID:-} != debian || ${VERSION_ID:-} != 13 ]]; then
  [[ $FORCE -eq 1 ]] || die "expected Debian 13, found ${PRETTY_NAME:-unknown}. Use --force to continue anyway."
  warn "not Debian 13 (${PRETTY_NAME:-unknown}); continuing because of --force"
fi

ARCH=$(dpkg --print-architecture)
BIN="$HERE/bin/mymicrotunnel-linux-$ARCH"
[[ -x $BIN ]] || die "no binary for $ARCH in this bundle (looked for $BIN)"

# --- packages ----------------------------------------------------------------

# wireguard-tools for wg(8), iproute2 for ip(8), sudo for the NOPASSWD rule
# that lets the owner move the tunnel, iputils-ping for the install's own check.
log "installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q --no-install-recommends \
  ca-certificates wireguard-tools iproute2 sudo iputils-ping kmod

if ! modprobe wireguard 2>/dev/null && [[ ! -d /sys/module/wireguard ]]; then
  warn "the wireguard kernel module did not load. Debian's stock kernel has it;"
  warn "a container or a custom kernel may not. Tunnels cannot come up without it."
fi

# --- files -------------------------------------------------------------------

# The helper is the copy the sudoers rule names, so it must be root-owned in a
# root-owned directory. The copy on PATH is a symlink to it: one binary, one
# version, nothing to drift.
log "installing $HELPER"
install -d -m 0755 -o root -g root "$(dirname "$HELPER")"
install -m 0755 -o root -g root "$BIN" "$HELPER.new"
mv -f "$HELPER.new" "$HELPER"
ln -sfn "$HELPER" "$COMMAND"

install -d -m 0755 "$DOC_DIR"
for doc in README.md LICENSE VERSION; do
  [[ -f $HERE/$doc ]] && install -m 0644 "$HERE/$doc" "$DOC_DIR/$doc"
done

# An existing supervisor is restarted so it runs the binary just installed.
if systemctl is-enabled --quiet mymicrotunnel-supervisor.service 2>/dev/null; then
  log "restarting the supervisor on the new binary"
  systemctl restart mymicrotunnel-supervisor.service
fi

log "installed $("$COMMAND" version)"
cat <<EOF

Next, as the user who owns the AWS credentials (not root):

  mymicrotunnel install --domain app.example.com --port 3000 --supervise

Then:
  sudo $HELPER tunnel up wg0       raise the tunnel
  sudo $HELPER tunnel down wg0     drop it
  mymicrotunnel status             check it
EOF
