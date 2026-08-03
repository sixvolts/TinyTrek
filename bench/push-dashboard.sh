#!/bin/sh
# Cross-build ttos-dashboard and swap it into the running DUT. Seconds, not minutes.
#
#   ./push-dashboard.sh                    build, install, restart
#   ./push-dashboard.sh --no-restart       stage it only
#   DUT=192.168.4.99 ./push-dashboard.sh   different target
#
# The bench doc's iteration loop names `./cmd/ctfd` and a `ttos-ctfd` service.
# NEITHER EXISTS. There is no separate CTF daemon: the challenge layer is inside
# the dashboard binary -- cmd/dashboard/ctf.go (identity, arm mask, heartbeat,
# flag frames) and cmd/dashboard/uds.go (the diagnostic server). One binary, one
# unit, one restart. Building `./cmd/ctfd` fails outright, which is the good case;
# restarting a nonexistent `ttos-ctfd` succeeds quietly and tests the OLD code,
# which is not.
#
# WHAT THIS CANNOT RELOAD, i.e. when you still owe a full image build:
#   - can0.network / can1.network        bus bitrates and FD mode
#   - the cangw policy + its unit        gateway rules
#   - ttos-provision, ttos-selftest      shell, but shipped by other recipes
#   - anything kernel, overlay, or layer
# Change one of those and push a binary over it and you are testing a DUT whose
# platform does not match the source tree. bench-status.sh reports the image
# vintage for exactly this reason -- check it when a result stops making sense.

set -e

DUT="${DUT:-192.168.4.133}"
DUT_USER="${DUT_USER:-ttos}"
# Factory/test-mode password (ttos-provision.sh sets ttos:ttos when no
# provisioning file is present). Fed to `sudo -S` on stdin rather than installing
# a NOPASSWD sudoers drop-in, so the DUT keeps stock privilege config and nothing
# has to be undone before it is reprovisioned or reused as a competition car.
DUT_PASS="${DUT_PASS:-ttos}"
SRC="$(cd "$(dirname "$0")/../meta-tt-ctf/recipes-apps/ttos-dashboard/files/src" && pwd)"
OUT="${TMPDIR:-/tmp}/ttos-dashboard.arm64"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=8"

cd "$SRC"

# Preflight: sudo on the DUT needs a password, and which password depends on
# whether the car has been provisioned. Factory/test mode is ttos/ttos;
# provisioning replaces it with the per-car console password. Fail here with a
# usable message rather than deep inside an scp or a systemctl.
if ! $SSH "$DUT_USER@$DUT" "echo '$DUT_PASS' | sudo -S -p '' true" 2>/dev/null; then
    printf 'cannot sudo on %s as %s.\n' "$DUT" "$DUT_USER" >&2
    printf 'If this car has been PROVISIONED the factory password no longer works:\n' >&2
    printf '  export DUT_PASS=<console password for this car, provisioning/OPERATOR-SECRETS.md>\n' >&2
    exit 1
fi

# CGO off: the DUT is a musl-free glibc Yocto rootfs, but a static pure-Go binary
# sidesteps every libc-version question. GOARCH=arm64 for the Pi 4.
printf 'building  %s\n' "$SRC"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o "$OUT" ./cmd/dashboard
printf 'built     %s  (%s bytes)\n' "$OUT" "$(stat -c%s "$OUT")"

printf 'pushing   %s@%s\n' "$DUT_USER" "$DUT"
scp -q -o BatchMode=yes "$OUT" "$DUT_USER@$DUT:/tmp/ttos-dashboard.new"

if [ "$1" = "--no-restart" ]; then
    printf 'staged    /tmp/ttos-dashboard.new (not installed)\n'
    exit 0
fi

# Rename over the target rather than writing through it: the running process keeps
# its old inode until the restart, so the cut-over is clean and a half-copied
# binary can never be what systemd execs. (install(1) is not on this rootfs --
# busybox is built without it, so cp + chmod + mv it is.)
$SSH "$DUT_USER@$DUT" "
    set -e
    S() { echo '$DUT_PASS' | sudo -S -p '' \"\$@\"; }
    chmod 0755 /tmp/ttos-dashboard.new
    S mv -f /tmp/ttos-dashboard.new /usr/bin/ttos-dashboard
    S systemctl restart ttos-dashboard
    sleep 1
    systemctl is-active ttos-dashboard
" || { printf 'restart FAILED -- journal:\n'; \
       $SSH "$DUT_USER@$DUT" "echo '$DUT_PASS' | sudo -S -p '' journalctl -u ttos-dashboard -n 30 --no-pager" 2>&1; exit 1; }

printf 'live      ttos-dashboard restarted on %s\n' "$DUT"
