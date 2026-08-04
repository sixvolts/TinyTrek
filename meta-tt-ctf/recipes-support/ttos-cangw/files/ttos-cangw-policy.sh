#!/bin/sh
# ttos-cangw-policy -- apply the CTF gateway policy between the two CAN buses.
#
# Role names, not numbers (the drive bus is the higher-numbered interface):
#   DRIVE = candrive   motors, BMS, Pi heartbeat.  Contestants only reach this in C3.
#   DIAG  = candiag   the side tap. Contestants live here.
#
# Default policy:
#   DRIVE -> DIAG   flag frames ONLY (0x7D1, 0x7D2)
#   DIAG  -> DRIVE  NOTHING
#
# Drive-bus traffic is deliberately NOT forwarded outbound. Forwarding
# 0x111/0x113/0x100/0x116 to the diagnostic bus would hand contestants the C2
# corpus by passive sniffing and collapse C1. The snapshot DID is the sanctioned
# corpus source.
#
# CLASSIC FRAMES ONLY -- never pass -X. The DRIVE bus is FD-configured but every
# node on it is a classic-only MCP2515; a real FD frame there is a form error that
# can drive the motor nodes to bus-off and stop propulsion.
set -eu

DRIVE_IF="${TTOS_CTF_DRIVE_IF:-candrive}"
DIAG_IF="${TTOS_CTF_DIAG_IF:-candiag}"
SFF_MASK="7FF"          # match the full standard 11-bit id
FLAG_IDS="7D1 7D2"      # C2 code, C3 code -- emitted by the BMS on the DRIVE bus

CANGW=$(command -v cangw || echo /usr/bin/cangw)
[ -x "$CANGW" ] || { echo "ttos-cangw: cangw not found (can-utils missing?)" >&2; exit 1; }

log() { echo "ttos-cangw: $*"; }

wait_for_iface() {
    # systemd-networkd may still be configuring the links. Bounded wait: a missing
    # interface must not hang boot, and a oneshot that fails is visible in status.
    n=0
    while [ "$n" -lt 30 ]; do
        [ -e "/sys/class/net/$1" ] && return 0
        n=$((n + 1)); sleep 1
    done
    return 1
}

flush() {
    # cangw rules are keyed on interface index and vanish with the interface, so a
    # flush on start is about clearing a PREVIOUS policy, not stale kernel state.
    "$CANGW" -F 2>/dev/null || true
}

apply_default() {
    for id in $FLAG_IDS; do
        "$CANGW" -A -s "$DRIVE_IF" -d "$DIAG_IF" -f "${id}:${SFF_MASK}" \
            || { echo "ttos-cangw: failed to add ${id} DRIVE->DIAG" >&2; return 1; }
    done
    log "default policy: $DRIVE_IF -> $DIAG_IF for $FLAG_IDS; nothing inbound"
}

case "${1:-default}" in
    default)
        wait_for_iface "$DRIVE_IF" || { echo "ttos-cangw: $DRIVE_IF never appeared" >&2; exit 1; }
        wait_for_iface "$DIAG_IF"  || { echo "ttos-cangw: $DIAG_IF never appeared" >&2; exit 1; }
        flush
        apply_default
        ;;
    flush)
        flush
        log "policy flushed"
        ;;
    show)
        "$CANGW" -L
        ;;
    *)
        echo "usage: ttos-cangw-policy [default|flush|show]" >&2
        exit 2
        ;;
esac
