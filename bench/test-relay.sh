#!/bin/sh
# C3 telematics relay -- exercised from the DUT itself.
#
# The relay binds the AP address ONLY, so it is unreachable from arcana over
# Ethernet. That is the property under test, not an inconvenience: a relay
# reachable from the wired network would be the socketcand hole again. So the
# client runs on the car, over loopback to the AP address.
#
#   ./test-relay.sh [car_id]

set -e
CAR="${1:-01}"
DUT="${DUT:-192.168.4.133}"
DUT_USER="${DUT_USER:-ttos}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=8"
RED='\033[31m'; GRN='\033[32m'; RST='\033[0m'
pass=0; fail=0
ck(){ if [ "$2" = "1" ]; then printf "  ${GRN}[PASS]${RST} %s\n" "$1"; pass=$((pass+1));
      else printf "  ${RED}[FAIL]${RST} %s\n" "$1"; fail=$((fail+1)); [ -n "$3" ] && printf "         %s\n" "$3"; fi; }

ROW=$(tr -d '\r' < "$REPO/provisioning/fleet-table.csv" | awk -F, -v c="$CAR" '$1==c {print; exit}')
VIN=$(printf '%s' "$ROW" | cut -d, -f6); SER=$(printf '%s' "$ROW" | cut -d, -f7)
SALT=$(grep -oE '[0-9a-f]{32}' "$REPO/provisioning/render-fleet.py" | head -1)
KEY=$(python3 -c "import hashlib,sys;print(hashlib.sha256(f'{sys.argv[1]}:{sys.argv[2]}:{sys.argv[3]}'.encode()).hexdigest()[:16])" "$VIN" "$SER" "$SALT")
ADDR=$($SSH "$DUT_USER@$DUT" 'grep -E "^TTOS_RELAY_ADDR=" /etc/default/ttos-dashboard | cut -d= -f2')

printf "\nC3 telematics relay   car %s   %s\n\n" "$CAR" "$ADDR"

# talk <lines...> : feed newline-separated commands, return the transcript
# busybox nc takes only "nc IPADDR PORT" -- no -w, no -q. The server closes the
# connection on QUIT, which is what ends the pipe; every command block below must
# therefore end in QUIT or this blocks.
# busybox nc takes only "nc IPADDR PORT" -- no -w, no -q. Fine for the
# single-command checks below, where nothing queues up.
#
# NOT fine for sustained traffic: nc exits on stdin EOF, and closing a socket with
# unread received data sends RST, discarding commands the server had not processed
# yet. Measured: 19 of 40 SEND commands reached the bus. test-c3.py uses a client
# that drains replies and gets 40 of 40.
talk() { $SSH "$DUT_USER@$DUT" "printf '%b' '$1' | nc ${ADDR%:*} ${ADDR##*:} 2>/dev/null"; }

# --- R1: reachable on the AP address, not on the wired one -------------------
OUT=$(talk 'PING\nQUIT\n' || true)
echo "$OUT" | grep -q "TTOS-TELEMATICS" && ck "R1  relay answers on the AP address" 1 \
    || ck "R1  relay answers on the AP address" 0 "$OUT"

WIRED=$(nc -w 3 "$DUT" "${ADDR##*:}" </dev/null 2>/dev/null | head -n 1 || true)
[ -z "$WIRED" ] && ck "R2  relay is NOT reachable over Ethernet (WiFi only)" 1 \
    || ck "R2  relay is NOT reachable over Ethernet (WiFi only)" 0 "got: $WIRED"

# --- R3: unauthenticated commands are refused --------------------------------
OUT=$(talk 'SEND 111#0000006601320132\nQUIT\n' || true)
echo "$OUT" | grep -q "ERR not authenticated" && ck "R3  SEND before AUTH is refused" 1 \
    || ck "R3  SEND before AUTH is refused" 0 "$OUT"

# --- R4: malformed vs wrong are distinguishable ------------------------------
OUT=$(talk 'AUTH nothex_zzzzzzzz\nQUIT\n' || true)
echo "$OUT" | grep -q "ERR malformed" && ck "R4a malformed key says so (fairness)" 1 \
    || ck "R4a malformed key says so (fairness)" 0 "$OUT"

OUT=$(talk 'AUTH 0000000000000000\nQUIT\n' || true)
echo "$OUT" | grep -q "ERR authentication failed" && ck "R4b well-formed but wrong key leaks nothing" 1 \
    || ck "R4b well-formed but wrong key leaks nothing" 0 "$OUT"

# --- R5: the derived key works ------------------------------------------------
OUT=$(talk "AUTH $KEY\nPING\nQUIT\n" || true)
echo "$OUT" | grep -q "OK authenticated" && ck "R5  key from SHA-256(vin:serial:salt)[:16] authenticates" 1 \
    || ck "R5  key from SHA-256(vin:serial:salt)[:16] authenticates" 0 "$OUT"

# --- R6: another car's key is rejected (anti-collusion) ----------------------
ROW2=$(tr -d '\r' < "$REPO/provisioning/fleet-table.csv" | awk -F, '$1=="02" {print; exit}')
V2=$(printf '%s' "$ROW2" | cut -d, -f6); S2=$(printf '%s' "$ROW2" | cut -d, -f7)
KEY2=$(python3 -c "import hashlib,sys;print(hashlib.sha256(f'{sys.argv[1]}:{sys.argv[2]}:{sys.argv[3]}'.encode()).hexdigest()[:16])" "$V2" "$S2" "$SALT")
OUT=$(talk "AUTH $KEY2\nQUIT\n" || true)
echo "$OUT" | grep -q "ERR authentication failed" && ck "R6  car 02's key does not open car 01" 1 \
    || ck "R6  car 02's key does not open car 01" 0 "$OUT"

# --- R7: no filesystem or command surface ------------------------------------
OUT=$(talk "AUTH $KEY\nCAT /etc/ttos/provision.src\nEXEC sh\nOPEN /etc/shadow\nQUIT\n" || true)
if echo "$OUT" | grep -q "TTOS_DATAID\|root:\|/bin/sh"; then
    ck "R7  no filesystem or command surface" 0 "$OUT"
else
    ck "R7  no filesystem or command surface" 1
fi

# --- R8: rate limiting --------------------------------------------------------
i=0; while [ $i -lt 6 ]; do talk 'AUTH 0000000000000000\nQUIT\n' >/dev/null 2>&1 || true; i=$((i+1)); done
OUT=$(talk 'PING\nQUIT\n' || true)
echo "$OUT" | grep -q "throttled" && ck "R8  repeated auth failures are throttled" 1 \
    || ck "R8  repeated auth failures are throttled" 0 "$OUT"

# --- R9 / C6: the full C3 solve -- authenticate, then drive until detected -----
# Uses a proper client that drains replies, NOT nc: busybox nc exits on stdin EOF
# and the resulting RST discards unprocessed commands (19 of 40 reached the bus in
# testing). That is a client artifact, but it makes nc useless for sustained work.
scp -q -o BatchMode=yes "$(dirname "$0")/lib/relaycli.py" "$DUT_USER@$DUT:/tmp/" 2>/dev/null
sleep 31   # let the R8 throttle window expire
CMDS=$(python3 - "$KEY" <<'PYEOF'
import hashlib, struct, sys
sys.path.insert(0, "lib")
from ttoscan import crc8_j1850
key = sys.argv[1]
def fr(did, steps, d, rpm, n):
    b = struct.pack(">I", steps) + bytes([d, rpm, n])
    return (b + bytes([crc8_j1850(struct.pack("<H", did) + b)])).hex().upper()
out = [f"AUTH {key}"]
for n in range(40):
    out.append(f"SEND 111#{fr(0x60B0,100,0x01,100,n)}")
    out.append(f"SEND 113#{fr(0x045C,100,0x01,100,n)}")
out.append("QUIT")
print("\n".join(out))
PYEOF
)
# THE OBSERVER MUST ACTUALLY OVERLAP THE CLIENT. This was written as
#   FLAGS=$(python3 ... &)
# which does not background anything useful: command substitution blocks until the
# pipe closes, so the 9-second capture ran to completion BEFORE relaycli was
# launched. FLAGS came back empty every time -- and nothing asserted on it, so the
# suite printed "all 8 relay assertions passed" regardless.
#
# That is why D1-D7 reached the fleet: this suite has never once verified that C3
# is solvable end to end. Redirect to a file, keep the real PID, and wait on it.
OBSOUT=$(mktemp)
python3 - > "$OBSOUT" 2>/dev/null <<'PYEOF' &
import sys
sys.path.insert(0, "lib")
from ttoscan import DIAG, Observer
o = Observer(DIAG); o.drain()
got = o.collect(12.0, match=lambda f: f.can_id in (0x7D1, 0x7D2))
o.close()
print(" ".join(sorted({hex(f.can_id) for f in got})))
PYEOF
OBSPID=$!
sleep 1
printf '%s' "$CMDS" | $SSH "$DUT_USER@$DUT" "python3 /tmp/relaycli.py ${ADDR%:*} ${ADDR##*:}" >/dev/null 2>&1 || true
wait "$OBSPID" 2>/dev/null || true
FLAGS=$(cat "$OBSOUT" 2>/dev/null); rm -f "$OBSOUT"

# --- R9 / C6: assert it. This is the end-to-end proof that the whole chain works:
# key derivation, relay auth, the protection CRC, the drive bus, the BMS detector,
# and the cangw rule that carries 0x7D2 back to the contestant's tap. Any one of
# them broken makes the car unsolvable, and every one of them has been broken at
# least once during development.
case "$FLAGS" in
    *0x7d2*) ck "R9  sustained forged commanding trips C3 and 0x7D2 reaches DIAG" 1 ;;
    *0x7d1*) ck "R9  sustained forged commanding trips C3 and 0x7D2 reaches DIAG" 0 \
                "saw only $FLAGS -- C2 fired but C3 did not. Check the BMS c3Count/c3WindowMs tuning and that pivotRpm matches the Pi." ;;
    *)       ck "R9  sustained forged commanding trips C3 and 0x7D2 reaches DIAG" 0 \
                "NO flag frames on DIAG at all. Either the forged frames never reached the drive bus, or the BMS detectors are stood down (run ttos-reset), or the cangw 0x7D1/0x7D2 rules are missing." ;;
esac
printf "\n%s\n" "$( [ "$fail" = 0 ] && printf "${GRN}all %d relay assertions passed${RST}" "$pass" \
                                   || printf "${RED}%d of %d failed${RST}" "$fail" "$((pass+fail))" )"
[ "$fail" = 0 ]
