#!/bin/sh
# ttos-provision-nodes -- push this car's Data IDs, unlock codes and detector
# thresholds to the three CAN nodes, which store them in flash.
#
#     sudo ttos-provision-nodes
#
# YOU DO NOT NORMALLY NEED TO RUN THIS. The car does it automatically on the first
# boot after it is provisioned -- drop ttos-provision.conf on the boot partition,
# power on, done. Shelling into eight cars is not a setup step.
#
# Run it by hand only when a NODE IS REPLACED after that first boot, since the
# automatic push happens once and is not repeated. The values it sends are the
# answer to Challenge 3; they must not be on the bus while a contestant is on it,
# which is exactly why it does not repeat on every restart.
#
# A node that has already been provisioned IGNORES what this sends, so running it
# again is harmless but also does nothing. To re-provision a node you have to erase
# it deliberately or reflash it -- that is the point: if a node could be
# reconfigured over the bus, anyone who reached the bus could hand it a Data ID they
# already knew and forge freely.
#
# Run it after a node is replaced, and after any change to the unlock codes or the
# pivot rpm in provisioning.

set -e
PATH="/sbin:/usr/sbin:$PATH"; export PATH

if [ "$(id -u)" != 0 ]; then
    if command -v sudo >/dev/null 2>&1; then exec sudo -- "$0" "$@"; fi
    echo "must run as root" >&2; exit 1
fi

# LOOPBACK, NOT THE AP ADDRESS. /api/provision-nodes is an operator action that
# sprays both Data IDs and the C2/C3 codes onto the drive bus, so the dashboard
# refuses it from anywhere but 127.0.0.1 -- otherwise any contestant associated to
# the AP could trigger it, and (before publishFrame filtered 0x101) read the codes
# straight off the DRIVE tab their C2 solve had already unlocked.
#
# Only the PORT is taken from the config; the host is fixed here.
ADDR=$(sed -n 's/^TTOS_DASH_ADDR=//p' /etc/default/ttos-dashboard 2>/dev/null | head -n 1)
PORT=${ADDR##*:}
case "$PORT" in ''|*[!0-9]*) PORT=80 ;; esac
ADDR="127.0.0.1:${PORT}"
CAR=$(grep -E '^TTOS_CAR_ID=' /etc/ttos/provision.src 2>/dev/null | cut -d= -f2)

printf '\n=== provisioning CAN nodes -- car %s ===\n\n' "${CAR:-UNPROVISIONED}"

if [ ! -f /etc/ttos/provision.src ]; then
    printf '  This car has no provisioned identity, so there is nothing to push.\n'
    printf '  Provision the Pi first (drop ttos-provision.conf on the boot partition).\n'
    printf '  The nodes stay permissive meanwhile, so the car still drives.\n\n'
    exit 1
fi

printf '  pushing to the drive bus (about 6 seconds) ... '
RESP=$(curl -s --max-time 30 -X POST "http://${ADDR}/api/provision-nodes" 2>/dev/null)
case "$RESP" in
    *'"ok":true'*) printf 'done\n' ;;
    '')            printf 'FAILED\n\n  No response from the console on %s.\n  Is ttos-dashboard running?\n\n' "$ADDR"; exit 1 ;;
    *)             printf 'FAILED\n\n  %s\n\n' "$RESP"; exit 1 ;;
esac

printf '\n  The nodes have stored their values and will ignore further config.\n'
printf '  Verify: the motors should now reject unprotected commands, and the BMS\n'
printf '  should emit its flag frames when a detection condition is met.\n\n'
