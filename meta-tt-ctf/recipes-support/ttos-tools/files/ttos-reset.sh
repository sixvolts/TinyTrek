#!/bin/sh
# ttos-reset -- return this car to a fresh, fully locked state between teams.
#
#     sudo ttos-reset
#
# RUNNABLE FROM THE SERIAL CONSOLE WITH NO LAPTOP. That is the requirement, not a
# convenience: between rounds an operator has a car, a USB cable and thirty
# seconds. Anything needing a browser or an SSH client will not get run, and the
# next team walks up to a station the previous team left fully unlocked.
#
# What it clears, and why restarting the dashboard is sufficient for most of it:
#   - every browser session's unlock tier   (in memory -- dies with the process)
#   - the car-level "tiers redeemed" record (in memory)
#   - any open C2 bridge window             (in memory)
#   - the BMS detector arm mask             (follows the car record, so it re-arms
#                                            on the next heartbeat, within 200 ms)
# The gateway policy is reloaded too, so a car left in an odd state by manual
# poking comes back to the shipped DRIVE->DIAG rules.

set -e
PATH="/sbin:/usr/sbin:$PATH"; export PATH

if [ "$(id -u)" != 0 ]; then
    if command -v sudo >/dev/null 2>&1; then exec sudo -- "$0" "$@"; fi
    echo "must run as root" >&2; exit 1
fi

CAR=$(grep -E '^TTOS_CAR_ID=' /etc/ttos/provision.src 2>/dev/null | cut -d= -f2)
printf '\n=== TTOS RESET -- car %s ===\n' "${CAR:-unprovisioned}"

printf '  restarting ttos-dashboard ... '
systemctl restart ttos-dashboard && printf 'ok\n' || { printf 'FAILED\n'; exit 1; }

printf '  reloading gateway policy  ... '
systemctl restart ttos-cangw && printf 'ok\n' || printf 'FAILED\n'

# Wait for the console to answer again, so the operator is not told "done" while
# the car is still coming up.
i=0
while [ "$i" -lt 15 ]; do
    if systemctl is-active --quiet ttos-dashboard; then break; fi
    i=$((i+1)); sleep 1
done

RULES=$(cangw -L 2>/dev/null | grep -c '^cangw -A' || true)
# `|| true` is REQUIRED on both: systemctl is-active exits non-zero for anything
# that is not active, and under `set -e` a failing command substitution in an
# assignment aborts the script. That is not merely untidy -- it exits before the
# diagnostic block below, so the operator gets a bare non-zero status and none of
# the explanation of what is wrong or what to do about it. Measured: a masked
# ttos-cangw exited 3 right here, silently.
DASH=$(systemctl is-active ttos-dashboard || true)
GW=$(systemctl is-active ttos-cangw || true)
printf '\n  dashboard : %s\n' "$DASH"
printf '  gateway   : %s (%s rules)\n' "$GW" "$RULES"
printf '  unlocks   : cleared\n'
printf '  detectors : re-armed\n\n'

# VERIFY BEFORE DECLARING. Every line above used to print unconditionally and the
# script exited 0 no matter what, so a failed cangw restart produced a car that
# boots, serves the panel, answers every diagnostic request and pivots correctly --
# and never awards a code, because ttos-cangw-policy flushes the table before
# re-applying it, and 0x7D1/0x7D2 then have no route from DRIVE to DIAG. The next
# team gets a station that is perfect except for the one thing they came for.
#
# bench/lib/carreset.py keys its guard on these exact strings, so the bench read
# the same lie back.
FAIL=0
[ "$DASH" = "active" ] || { printf '  XX dashboard is %s, not active\n' "$DASH"; FAIL=1; }
[ "$GW" = "active" ]   || { printf '  XX gateway is %s, not active\n' "$GW"; FAIL=1; }
# Two rules: 0x7D1 and 0x7D2, DRIVE->DIAG. Fewer means flag frames cannot reach
# the contestant's tap, which is indistinguishable from an unsolvable challenge.
[ "${RULES:-0}" -ge 2 ] 2>/dev/null || {
    printf '  XX gateway has %s rule(s), expected 2 (0x7D1 and 0x7D2 DRIVE->DIAG)\n' "${RULES:-0}"
    printf '     Flag frames cannot reach the diagnostic bus. Challenges 2 and 3\n'
    printf '     will look solvable and award nothing. Check: systemctl status ttos-cangw\n'
    FAIL=1
}

if [ "$FAIL" -ne 0 ]; then
    printf '\n  *** RESET INCOMPLETE -- DO NOT hand this car to the next team. ***\n\n'
    exit 1
fi

printf '  This car is LOCKED and ready for the next team.\n\n'
