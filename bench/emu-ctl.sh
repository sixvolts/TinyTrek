#!/bin/sh
# Talk to a running vehicle-emulator.py over its control socket.
#
#   ./emu-ctl.sh status
#   ./emu-ctl.sh drop 20            drop 20% of received frames
#   ./emu-ctl.sh mute on            stop transmitting entirely (node goes quiet)
#   ./emu-ctl.sh latency 50         delay every transmission by 50 ms
#   ./emu-ctl.sh lvc on             latch low-voltage cutoff, rail off
#   ./emu-ctl.sh vbat 9200          park the pack voltage
#   ./emu-ctl.sh sweep 8300 12400 60   triangle-sweep the gauge range over 60 s
#   ./emu-ctl.sh rail on|off        force the 12V rail without a 0x115
#   ./emu-ctl.sh reset              clear every injected fault
#
# `mute on` makes the node stop TRANSMITTING, which is not the same as bus-off:
# the controller still ACKs, so the DUT's writes keep succeeding. To reproduce a
# genuinely absent node -- the ENOBUFS case that has bitten this project
# repeatedly -- take the interface down instead:
#     sudo ip link set ttdrive down
# Nothing in userspace can withhold an ACK.

SOCK="${TTOS_EMU_SOCK:-/tmp/ttos-bench-emu.sock}"

[ -S "$SOCK" ] || { echo "no emulator control socket at $SOCK -- is it running?" >&2; exit 1; }
[ $# -gt 0 ] || set -- status

printf '%s' "$*" | { nc -U -q1 "$SOCK" 2>/dev/null || socat - "UNIX-CONNECT:$SOCK" 2>/dev/null; }
