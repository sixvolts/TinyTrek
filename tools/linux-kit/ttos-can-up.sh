#!/usr/bin/env bash
# ttos-can-up.sh
#
# Bring a PEAK PCAN-USB (Pro) FD adapter up on Linux via socketcan and find the
# diagnostic bus, so ttos-ctf-tool.py can talk to a TTOS CTF car.
#
# This is the SAME path the bench host (arcana) uses, where it is known-good. On
# Linux the PEAK adapter binds the in-kernel peak_usb driver -- no MacCAN, no
# libPCBUSB, none of the FD-receive trouble the macOS path had.
#
# Run it:   ./ttos-can-up.sh          (it re-execs itself under sudo)
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOL="${TTOS_TOOL:-$HERE/ttos-ctf-tool.py}"

# The car's diagnostic bus: 500 kbit arbitration, 1 Mbit data, CAN FD, ISO.
# This is EXACTLY the command the bench host (arcana) uses and that is known to
# work -- no explicit sample points; the kernel picks them from the controller,
# which on this hardware lands at 87.5%/75% anyway. Do not "improve" it.
BITRATE=500000
DBITRATE=1000000
SALT="dd1b35d820f614e9c94f6a0e4f34cbc3"

say() { printf '  %s\n' "$*"; }
die() { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

[ -f "$TOOL" ] || die "ttos-ctf-tool.py not found beside this script. Put them in the same folder, or set TTOS_TOOL=/path/to/ttos-ctf-tool.py"
command -v python3 >/dev/null || die "python3 is required (apt install python3)."

# Raw CAN sockets and 'ip link' both need root. Re-exec once, preserving env.
if [ "$(id -u)" != 0 ]; then
    exec sudo -E "$0" "$@"
fi

echo "== 1. PEAK driver =="
modprobe peak_usb 2>/dev/null || true
if command -v lsusb >/dev/null && lsusb | grep -qi 'peak\|1ac[0-9a-f]'; then
    say "PEAK adapter visible on USB."
else
    say "Could not positively see a PEAK adapter via lsusb -- continuing anyway."
    say "(lsusb may be absent, or the vendor string may differ; the interface check below is what matters.)"
fi

echo
echo "== 2. CAN interfaces =="
# A dual-channel Pro FD shows up as TWO interfaces (e.g. can0 and can1); a
# single-channel adapter as one. We do not care what they are named.
mapfile -t IFACES < <(ip -o link show type can 2>/dev/null | awk -F': ' '{print $2}' | cut -d'@' -f1)
if [ "${#IFACES[@]}" -eq 0 ]; then
    die "No CAN interfaces present.
  - Is the adapter plugged in?
  - Did peak_usb load?  Check:  dmesg | grep -i peak
  - On a very old kernel the driver may be missing entirely."
fi
say "found: ${IFACES[*]}"

echo
echo "== 3. bring up @ ${BITRATE}/${DBITRATE} CAN FD =="
for dev in "${IFACES[@]}"; do
    ip link set "$dev" down 2>/dev/null || true
    if ip link set "$dev" up type can \
         bitrate "$BITRATE" dbitrate "$DBITRATE" fd on 2>/dev/null; then
        say "$dev up (CAN FD ${BITRATE}/${DBITRATE})"
    else
        say "$dev FAILED to come up -- see: ip -details link show $dev"
    fi
done

echo
echo "== 4. find the diagnostic bus =="
# The DIAG bus answers a VIN read. The DRIVE bus does not (no diagnostic server),
# and an unplugged channel of a dual adapter does not either. A VIN read commands
# NO motion, so this is safe to try on every interface.
DIAG=""
for dev in "${IFACES[@]}"; do
    out="$(python3 "$TOOL" --transport socketcan --iface "$dev" vin 2>/dev/null || true)"
    if printf '%s' "$out" | grep -q '^TTKTREK'; then
        DIAG="$dev"
        say "$dev -> VIN $out   <<< DIAGNOSTIC BUS"
    else
        say "$dev -> no VIN (drive bus, or not the connected channel)"
    fi
done

echo
if [ -n "$DIAG" ]; then
    cat <<EOF
================================================================
READY.  Diagnostic bus = $DIAG

Full challenge chain (this is the whole game, C1 -> C2 -> C3):
  sudo python3 $TOOL \\
       --transport socketcan --iface $DIAG \\
       --salt $SALT walk

Single steps:
  sudo python3 $TOOL --transport socketcan --iface $DIAG vin
  sudo python3 $TOOL --transport socketcan --iface $DIAG pivot      # car turns in place
  sudo python3 $TOOL --transport socketcan --iface $DIAG snapshot
  sudo python3 $TOOL --transport socketcan --iface $DIAG monitor    # passive listen

Anything odd: add --debug to see every frame in and out.
================================================================
EOF
else
    die "No interface answered a VIN.
  - Is the adapter on the car's DIAGNOSTIC port (not the drive bus)? CAN-H/CAN-L right?
  - Is the car powered and is ttos-dashboard running?
  - 'candump ${IFACES[0]}' staying silent until you send is NORMAL for the DIAG bus.
  Fix the cabling / car, then re-run."
fi
