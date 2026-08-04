#!/bin/sh
# Give the bench DUT a challenge IDENTITY without running full provisioning.
#
#   ./provision-bench.sh          # car 01 (must match the emulator's --car)
#   ./provision-bench.sh 03
#
# This stages /etc/ttos/provision.src only -- the file the dashboard reads for
# VIN, ECU serial, fleet salt, Data IDs and the three unlock codes. It does NOT
# run ttos-provision, deliberately: full provisioning also sets the hostname,
# switches the AP to WPA2, and replaces the ttos password with the fleet hash --
# which would break the factory
# password that every bench script feeds to `sudo -S`. The bench wants the
# identity, not the lockdown.
#
# Consequence to keep in mind: the DUT reports itself UNPROVISIONED and the
# self-test's section 1 and 10 keep failing. That is correct and expected here.
# It also means this is NOT a test of the provisioning path -- flash a real image
# and run ttos-provision properly before the fleet is built.
#
# THE CAR ID MUST MATCH vehicle-emulator.py's --car. Data IDs are per-car, so a
# mismatch makes every protected frame fail CRC in silence, which is
# indistinguishable from a bus fault.

set -e

CAR="${1:-01}"
DUT="${DUT:-192.168.4.133}"
DUT_USER="${DUT_USER:-ttos}"
DUT_PASS="${DUT_PASS:-ttos}"
CSV="$(cd "$(dirname "$0")/.." && pwd)/provisioning/fleet-table.csv"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=8"

[ -f "$CSV" ] || { echo "no $CSV (provisioning/ is gitignored -- restore it first)" >&2; exit 1; }

# fleet-table.csv is CRLF. Strip the CR or every trailing field carries one, which
# has already cost this project one debugging session on the password hashes.
ROW=$(tr -d '\r' < "$CSV" | awk -F, -v c="$CAR" '$1==c {print; exit}')
[ -n "$ROW" ] || { echo "car $CAR not in fleet-table.csv" >&2; exit 1; }

set -- $(printf '%s' "$ROW" | tr ',' ' ')
# car_id host ssid psk chan vin ecu_serial dataid_l dataid_r c1 c2 c3 hash
CAR_ID=$1; VIN=$6; ECU=$7; DID_L=$8; DID_R=$9
shift 9; C1=$1; C2=$2; C3=$3
SALT=$(grep -oE '^FLEET_SALT *= *"[0-9a-f]+"' "$(dirname "$CSV")/render-fleet.py" 2>/dev/null | grep -oE '[0-9a-f]{32}' | head -1)
[ -n "$SALT" ] || SALT="00000000000000000000000000000000"

TMP=$(mktemp)
cat > "$TMP" <<EOF
# TTOS CTF bench identity -- staged by bench/provision-bench.sh, NOT by
# ttos-provision. This car is deliberately still in factory mode.
TTOS_CAR_ID=$CAR_ID
TTOS_VIN=$VIN
TTOS_ECU_SERIAL=$ECU
TTOS_FLEET_SALT=$SALT
TTOS_DATAID_L=$DID_L
TTOS_DATAID_R=$DID_R
TTOS_CODE_C1=$C1
TTOS_CODE_C2=$C2
TTOS_CODE_C3=$C3
EOF


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
echo "staging identity for car $CAR_ID ($VIN) on $DUT"
scp -q -o BatchMode=yes "$TMP" "$DUT_USER@$DUT:/tmp/.provsrc"
rm -f "$TMP"
$SSH "$DUT_USER@$DUT" "
    S() { echo '$DUT_PASS' | sudo -S -p '' \"\$@\"; }
    S mkdir -p /etc/ttos
    S mv -f /tmp/.provsrc /etc/ttos/provision.src
    S groupadd -f ttos-secrets 2>/dev/null || true
    S chown root:ttos-secrets /etc/ttos/provision.src 2>/dev/null || S chown root:root /etc/ttos/provision.src
    S chmod 640 /etc/ttos/provision.src
    S systemctl restart ttos-dashboard
    sleep 1
    systemctl is-active ttos-dashboard
"
$SSH "$DUT_USER@$DUT" "echo '$DUT_PASS' | sudo -S -p '' journalctl -u ttos-dashboard -n 20 --no-pager" 2>/dev/null \
    | grep -iE "identity|C1|challenge" | tail -3
echo "done -- emulator must be running with --car $CAR_ID"
