#!/bin/sh
# Sync the PLATFORM half of the DUT -- the files a Go rebuild cannot touch --
# straight from the source tree, and reconfigure the interfaces they describe.
#
#   ./push-config.sh              sync, reconfigure, verify
#   ./push-config.sh --dry-run    show what differs, change nothing
#
# push-dashboard.sh keeps the DUT's userspace current in ~2 s. Everything else --
# bus bitrates, FD mode, the gateway policy and its unit, provisioning, the
# self-test -- ships in the image, so the DUT used to drift away from git until
# someone pulled the SD card. These are all just files on the rootfs; there is no
# reason a card swap has to be in the loop.
#
# WHAT THIS STILL CANNOT DO: kernel, device-tree overlays, layer/recipe changes,
# package sets, and anything on the FAT boot partition. A file that lands in the
# image via IMAGE_BOOT_FILES or a kernel fragment needs a real flash. This covers
# rootfs config and scripts, which is where nearly all challenge-layer churn is.
#
# It also does NOT make the DUT equal to a freshly flashed image -- it makes the
# files in the manifest equal. Before any milestone or go/no-go, flash the real
# .wic and re-run the suite; treat this as the iteration path, not the sign-off.

set -e

DUT="${DUT:-192.168.4.133}"
DUT_USER="${DUT_USER:-ttos}"
DUT_PASS="${DUT_PASS:-ttos}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=8"
DRY=0
[ "$1" = "--dry-run" ] && DRY=1

RED='\033[31m'; GRN='\033[32m'; YLW='\033[33m'; BLD='\033[1m'; RST='\033[0m'

# src (relative to repo root)                                            dest                                        mode
MANIFEST="
meta-tt-ctf/recipes-core/systemd/systemd/can0.network|/etc/systemd/network/can0.network|0644
meta-tt-ctf/recipes-core/systemd/systemd/can1.network|/etc/systemd/network/can1.network|0644
meta-tt-ctf/recipes-core/systemd/systemd/10-can0.link|/etc/systemd/network/10-can0.link|0644
meta-tt-ctf/recipes-core/systemd/systemd/10-can1.link|/etc/systemd/network/10-can1.link|0644
meta-tt-ctf/recipes-core/systemd/systemd/30-wlan-ap.network|/etc/systemd/network/30-wlan-ap.network|0644
meta-tt-ctf/recipes-support/ttos-cangw/files/ttos-cangw-policy.sh|/usr/bin/ttos-cangw-policy|0755
meta-tt-ctf/recipes-support/ttos-cangw/files/ttos-cangw.service|/lib/systemd/system/ttos-cangw.service|0644
meta-tt-ctf/recipes-support/ttos-tools/files/ttos-selftest.sh|/usr/bin/ttos-selftest|0755
meta-tt-ctf/recipes-support/ttos-tools/files/ttos-reset.sh|/usr/bin/ttos-reset|0755
meta-tt-ctf/recipes-support/ttos-tools/files/ttos-provision-nodes.sh|/usr/bin/ttos-provision-nodes|0755
meta-tt-ctf/recipes-core/systemd/systemd/ttos-journald.conf|/etc/systemd/journald.conf.d/ttos-journald.conf|0644
meta-tt-ctf/recipes-provision/ttos-provision/files/ttos-provision.sh|/usr/bin/ttos-provision|0755
meta-tt-ctf/recipes-apps/ttos-dashboard/files/ttos-dashboard.default|/etc/default/ttos-dashboard|0644
meta-tt-ctf/recipes-apps/ttos-dashboard/files/ttos-dashboard.service|/lib/systemd/system/ttos-dashboard.service|0644
"

R() { $SSH "$DUT_USER@$DUT" "echo '$DUT_PASS' | sudo -S -p '' $*" 2>/dev/null; }


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
printf "${BLD}== comparing source tree to %s ==${RST}\n" "$DUT"
changed=""
for row in $MANIFEST; do
    src="${row%%|*}"; rest="${row#*|}"; dest="${rest%%|*}"; mode="${rest##*|}"
    [ -f "$REPO/$src" ] || { printf "${RED}  MISSING SOURCE %s${RST}\n" "$src"; exit 1; }
    lsum=$(sha256sum "$REPO/$src" | cut -d' ' -f1)
    rsum=$($SSH "$DUT_USER@$DUT" "sha256sum '$dest' 2>/dev/null | cut -d' ' -f1" 2>/dev/null)
    if [ "$lsum" = "$rsum" ]; then
        printf "  same     %s\n" "$dest"
    else
        printf "${YLW}  DIFFERS  %s${RST}\n" "$dest"
        changed="$changed $src|$dest|$mode"
    fi
done

if [ -z "$changed" ]; then
    printf "${GRN}DUT config already matches the source tree.${RST}\n"
    exit 0
fi
if [ "$DRY" = 1 ]; then
    printf "\n${YLW}--dry-run: nothing changed.${RST}\n"
    exit 0
fi

printf "\n${BLD}== pushing ==${RST}\n"
for row in $changed; do
    src="${row%%|*}"; rest="${row#*|}"; dest="${rest%%|*}"; mode="${rest##*|}"
    base=$(basename "$dest")
    scp -q -o BatchMode=yes "$REPO/$src" "$DUT_USER@$DUT:/tmp/.push.$base"
    R "sh -c 'chmod $mode /tmp/.push.$base && mv -f /tmp/.push.$base $dest'"
    printf "  -> %s\n" "$dest"
done

printf "\n${BLD}== reconfiguring ==${RST}\n"
# CAN link parameters (bitrate, FD mode) can ONLY be set while the interface is
# DOWN. systemd-networkd will not bounce a link just because its .network changed,
# so `networkctl reload` alone leaves an FD interface FD forever and the push looks
# like it did nothing. Take them down first, then reconfigure.
#
# Stopping ttos-cangw first is deliberate: its ExecStop flushes the rules, and
# rules that reference an interface being bounced are better rebuilt than trusted.
R systemctl stop ttos-cangw >/dev/null 2>&1 || true
R "sh -c 'PATH=/sbin:/usr/sbin:\$PATH; ip link set can0 down; ip link set can1 down'" || true
R systemctl daemon-reload
R networkctl reload >/dev/null 2>&1 || true
R "sh -c 'PATH=/sbin:/usr/sbin:\$PATH; networkctl reconfigure can0 can1'" >/dev/null 2>&1 || true
sleep 3

# networkd does not necessarily clear FD state it did not set, so verify against
# the kernel and force where needed.
#
# WHICH interface must be classic is DERIVED from the shipped .network files rather
# than hard-coded. It was hard-coded to can1 until the bus roles were corrected on
# 2026-08-03, at which point this silently stopped forcing anything and left the
# drive bus in FD mode -- the exact hazard the check exists to catch.
for IF in can0 can1; do
    SRC="$REPO/meta-tt-ctf/recipes-core/systemd/systemd/${IF}.network"
    [ -f "$SRC" ] || continue
    if grep -q '^FDMode=yes' "$SRC"; then
        continue    # this one is meant to be FD
    fi
    if R "sh -c 'PATH=/sbin:/usr/sbin:\$PATH; ip -d link show $IF'" | grep -q dbitrate; then
        printf "${YLW}  %s still FD after reconfigure -- forcing it classic${RST}\n" "$IF"
        R "sh -c 'PATH=/sbin:/usr/sbin:\$PATH; ip link set $IF down; ip link set $IF up type can bitrate 500000 fd off'"
    fi
done

R systemctl restart ttos-cangw || true
R systemctl restart ttos-dashboard || true
sleep 2

printf "\n${BLD}== verify ==${RST}\n"
R "sh -c 'PATH=/sbin:/usr/sbin:\$PATH
  for i in can0 can1; do
    printf \"  %s  %s\n\" \"\$i\" \"\$(ip -d link show \$i | grep -oE \"bitrate [0-9]+|dbitrate [0-9]+|can state [A-Z-]+\" | tr \"\n\" \" \")\"
  done
  echo \"  cangw     \$(systemctl is-active ttos-cangw)   rules: \$(cangw -L 2>/dev/null | grep -c \"^cangw -A\")\"
  echo \"  dashboard \$(systemctl is-active ttos-dashboard)\"'"
printf "\n"
