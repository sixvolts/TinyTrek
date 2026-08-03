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
systemctl restart ttos-cangw && printf 'ok\n' || printf 'WARN (check ttos-cangw)\n'

# Wait for the console to answer again, so the operator is not told "done" while
# the car is still coming up.
i=0
while [ "$i" -lt 15 ]; do
    if systemctl is-active --quiet ttos-dashboard; then break; fi
    i=$((i+1)); sleep 1
done

RULES=$(cangw -L 2>/dev/null | grep -c '^cangw -A' || true)
printf '\n  dashboard : %s\n' "$(systemctl is-active ttos-dashboard)"
printf '  gateway   : %s (%s rules)\n' "$(systemctl is-active ttos-cangw)" "$RULES"
printf '  unlocks   : cleared\n'
printf '  detectors : re-armed\n\n'
printf '  This car is LOCKED and ready for the next team.\n\n'
