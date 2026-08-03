#!/bin/sh
# TTOS CTF bench rig -- bring both bus adapters up and PROVE the role mapping.
#
# Usage:  sudo ./bench-up.sh            bring up + verify
#         sudo ./bench-up.sh down       take both down
#         DRIVE_FD=1 sudo ./bench-up.sh bring DRIVE up as FD (see below)
#
# The verify step is not decoration. Every negative assertion in the test matrix
# ("0x115 never crosses to DRIVE", "drive traffic never reaches DIAG") is a claim
# about WHICH WIRE a frame did not appear on. If the adapters are swapped, all of
# those pass for the wrong reason and the suite is green over a broken gateway.
# So we confirm the mapping against live traffic before anything else runs.

set -e

DRIVE_IF="${TTOS_BENCH_DRIVE_IF:-ttdrive}"
DIAG_IF="${TTOS_BENCH_DIAG_IF:-ttdiag}"
ARB_BITRATE=500000
DATA_BITRATE=1000000

red()  { printf '\033[31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
ylw()  { printf '\033[33m%s\033[0m\n' "$*"; }

if [ "$(id -u)" != 0 ]; then
    if command -v sudo >/dev/null 2>&1; then exec sudo -- "$0" "$@"; fi
    red "must run as root"; exit 1
fi

# ---------------------------------------------------------------- down --------
if [ "$1" = "down" ]; then
    for i in "$DRIVE_IF" "$DIAG_IF"; do
        ip link set "$i" down 2>/dev/null && echo "$i down" || echo "$i already down"
    done
    exit 0
fi

# ---------------------------------------------------------------- exist -------
missing=0
for i in "$DRIVE_IF" "$DIAG_IF"; do
    if ! ip link show "$i" >/dev/null 2>&1; then
        red "interface $i does not exist"
        missing=1
    fi
done
if [ "$missing" = 1 ]; then
    ylw "Present CAN interfaces: $(ip -br link show type can 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
    ylw "If these are still can0/can1, the udev rules are not installed:"
    ylw "  sudo cp bench/udev/99-ttos-bench-can.rules /etc/udev/rules.d/"
    ylw "  sudo udevadm control --reload && sudo udevadm trigger -s net"
    exit 1
fi

# ---------------------------------------------------------------- up ----------
# DIAG is CAN FD: 500 kbit arbitration / 1 Mbit data. This is the contestant-facing
# bus and the one that carries 64-byte UDS responses.
ip link set "$DIAG_IF" down 2>/dev/null || true
ip link set "$DIAG_IF" up type can bitrate "$ARB_BITRATE" dbitrate "$DATA_BITRATE" fd on
grn "$DIAG_IF  up   DIAG   FD ${ARB_BITRATE}/${DATA_BITRATE}"

# DRIVE is CLASSIC by default, deliberately.
#
# The real nodes on the drive bus are MCP2515s, which have no FD support at all.
# Configuring this adapter classic makes it behave like they do: a genuine FD frame
# on the drive bus becomes a form error here, exactly as it would on a motor node.
# That turns the adapter into a CANARY for the FD hazard (matrix G3) instead of an
# FD-tolerant observer that would quietly absorb the very frame we are trying to
# prove never arrives.
#
# Set DRIVE_FD=1 only when you specifically need to CAPTURE a stray FD frame's
# contents rather than detect that one occurred.
ip link set "$DRIVE_IF" down 2>/dev/null || true
if [ "${DRIVE_FD:-0}" = "1" ]; then
    ip link set "$DRIVE_IF" up type can bitrate "$ARB_BITRATE" dbitrate "$DATA_BITRATE" fd on
    ylw "$DRIVE_IF up   DRIVE  FD ${ARB_BITRATE}/${DATA_BITRATE}  <-- NOT the default; G3 cannot be trusted in this mode"
else
    # `fd off` is NOT redundant. Setting bitrate alone leaves whatever FD state the
    # interface last had, so an adapter used as DIAG earlier -- or in a previous
    # bench session -- comes back up FD-capable while this script reports CLASSIC.
    # It then absorbs a stray FD frame instead of erroring on it, and the canary is
    # gone with no outward sign. Observed on this rig 2026-08-03.
    ip link set "$DRIVE_IF" up type can bitrate "$ARB_BITRATE" fd off
    grn "$DRIVE_IF up   DRIVE  CLASSIC ${ARB_BITRATE}"
fi

# Say what the kernel thinks, not what we asked for.
if ip -d link show "$DRIVE_IF" | grep -q 'dbitrate'; then
    if [ "${DRIVE_FD:-0}" != "1" ]; then
        red "FAIL  $DRIVE_IF still reports a data bitrate after 'fd off' -- it is NOT classic."
        red "      The FD canary is inoperative; matrix G3 would pass vacuously."
        exit 3
    fi
fi

# ---------------------------------------------------------------- verify ------
# The DUT dashboard transmits the 0x100 liveness heartbeat on its DRIVE bus every
# 200 ms, unconditionally, whether or not the panel is unlocked. The gateway policy
# forwards only 0x7D1/0x7D2 DRIVE->DIAG, so 0x100 must NEVER appear on DIAG. That
# makes it an unambiguous discriminator for which adapter is on which bus.
echo
echo "verifying role mapping against live traffic (1.5 s)..."

drive_hb=0; diag_hb=0
d1=$(timeout 1.6 candump -T 1500 "$DRIVE_IF",100:7FF 2>/dev/null | wc -l) || d1=0
d2=$(timeout 1.6 candump -T 1500 "$DIAG_IF",100:7FF  2>/dev/null | wc -l) || d2=0
[ "$d1" -gt 0 ] && drive_hb=1
[ "$d2" -gt 0 ] && diag_hb=1

if [ "$drive_hb" = 1 ] && [ "$diag_hb" = 0 ]; then
    grn "OK    0x100 heartbeat seen on $DRIVE_IF only -- roles are correct"
elif [ "$drive_hb" = 0 ] && [ "$diag_hb" = 1 ]; then
    red "FAIL  0x100 heartbeat is on $DIAG_IF -- THE ADAPTERS ARE SWAPPED."
    red "      Every negative assertion would pass for the wrong reason."
    red "      Swap the two USB cables, or swap the ID_PATH values in the udev rule."
    exit 2
elif [ "$drive_hb" = 1 ] && [ "$diag_hb" = 1 ]; then
    red "FAIL  0x100 seen on BOTH buses."
    red "      Either the two buses are shorted together, or a gateway rule is"
    red "      forwarding drive traffic to DIAG (matrix G2 violation)."
    exit 2
else
    ylw "WARN  no 0x100 heartbeat on either bus -- role mapping NOT verified."
    ylw "      Is the DUT powered and ttos-dashboard running? Check:"
    ylw "        ssh ttos@\$DUT systemctl status ttos-dashboard"
    ylw "      Treat cross-bus results as unproven until this passes."
fi

echo
ip -br link show "$DRIVE_IF" "$DIAG_IF" 2>/dev/null || true
