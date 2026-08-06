#!/bin/sh
# TTOS assembly bring-up -- prove a robot is wired and built correctly.
#
# RUN THIS ON THE CAR, as root:   sudo ttos-bringup
#
# ---------------------------------------------------------------------------
# WHY THIS EXISTS
#
# A car with no ttos-provision.conf boots into FACTORY/TEST mode: open AP
# "TTOS-TEST", console login ttos/ttos, and drive controls OPEN on the panel --
# that mode exists precisely so a freshly-assembled car can be exercised before it
# has any challenge identity.
#
# What is still unavailable there, correctly, is the challenge layer: the pivot
# routine returns NRC 0x22 and the panel rejects every flag, because there is no
# identity to award. So ttos-selftest and `ttos-ctf-tool.py walk` cannot accept an
# unprovisioned car -- and those are the tools that would otherwise tell you the
# robot is wired right.
#
# This script fills that gap, and covers three things the panel cannot:
#
#   ONE WHEEL AT A TIME    a swapped pair of motor-node CAN connectors is
#                          invisible if you command both wheels together, which
#                          is what every panel control does
#   BUS-LEVEL EVIDENCE     controller error states, and whether the BMS is
#                          actually talking, rather than "the car did not move"
#   NO BROWSER, NO AP      runs over SSH or the serial console, so you are not
#                          rejoining a different WiFi network for each of eight
#                          cars
#
# It talks to the nodes directly, which works because an unprovisioned motor node
# is PERMISSIVE: it has no Data ID, so it accepts any frame on its ID including
# the 6-byte legacy form (TinytrekLMotor.ino: `bool accept = true; if
# (haveDataId) {...}`).
#
# ONCE A CAR IS PROVISIONED THE NODES GO STRICT and ignore these frames silently
# -- no error, no reply, nothing. This script detects that and tells you to use
# ttos-selftest and ttos-ctf-tool.py walk instead. It does not pretend.
#
# ---------------------------------------------------------------------------
# WHAT IT CANNOT TELL YOU
#
# Nothing here proves a wheel turned. The nodes accept a command and report
# nothing back about the physical world -- there is no encoder. The motion steps
# are OPERATOR-CONFIRMED: you watch, you answer. A script that inferred "the
# wheels turned" from "the frame was accepted" would pass a car with the motor
# leads off, which is the single most common assembly fault on this build.

set -eu

# can-utils live in /usr/bin and are NOT on a login shell's PATH on this image;
# `sudo sh script` inherits that. Absolute-ish PATH rather than discovering at 3am
# that every bus check silently counted zero frames because candump was not found.
PATH=/usr/bin:/usr/sbin:/bin:/sbin:$PATH
export PATH

DRIVE="${TTOS_DRIVE_IF:-candrive}"
DIAG="${TTOS_DIAG_IF:-candiag}"
RPM="${TTOS_BRINGUP_RPM:-60}"
STEPS="${TTOS_BRINGUP_STEPS:-400}"
MOTION=1
[ "${1:-}" = "--no-motion" ] && MOTION=0

R=$(printf '\033[31m'); G=$(printf '\033[32m'); Y=$(printf '\033[33m')
B=$(printf '\033[1m');  X=$(printf '\033[0m')
pass=0; fail=0; warn=0

ok()   { pass=$((pass+1)); printf '  %sPASS%s  %s\n' "$G" "$X" "$1"; }
no()   { fail=$((fail+1)); printf '  %sFAIL%s  %s\n' "$R" "$X" "$1";
         [ $# -gt 1 ] && printf '        %s\n' "$2"; return 0; }
wrn()  { warn=$((warn+1)); printf '  %sWARN%s  %s\n' "$Y" "$X" "$1";
         [ $# -gt 1 ] && printf '        %s\n' "$2"; return 0; }
hdr()  { printf '\n%s%s%s\n' "$B" "$1" "$X"; }

# Count frames with a given ID, up to _want, giving up after _ms of silence.
#
# There is no `timeout` on this image, and it would be the wrong tool anyway:
# killing candump mid-run discards its block-buffered stdout, so a piped count
# comes back 0 whether or not frames arrived. candump's own -n/-T make it EXIT
# normally, which flushes. This project has already lost an afternoon to that
# exact confusion once.
count_id() {
    _if=$1; _id=$2; _want=$3; _ms=$4
    candump -n "$_want" -T "$_ms" "$_if","$_id":7FF 2>/dev/null | grep -c . || true
}

ask() {
    printf '\n  %s%s%s [y/N] ' "$B" "$1" "$X"
    read -r a </dev/tty || a=n
    case "$a" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------------------
hdr "0. Where are we"

if [ "$(id -u)" != 0 ]; then
    printf '%sRun me as root -- I transmit on the drive bus.%s\n' "$R" "$X"
    exit 2
fi

CARID=$(cat /etc/ttos/car-id 2>/dev/null || echo "")
printf '  car id      %s\n' "${CARID:-<none>}"
printf '  drive bus   %s\n' "$DRIVE"
printf '  diag bus    %s\n' "$DIAG"

# Provisioned or not? The dashboard says so at startup, loudly, on purpose.
PROVISIONED=0
if journalctl -u ttos-dashboard -b 2>/dev/null | grep -q "CTF identity loaded"; then
    PROVISIONED=1
fi
MISSING=$(journalctl -u ttos-dashboard -b 2>/dev/null |
          sed -n 's/.*CTF identity INCOMPLETE -- missing \(.*\)\. Challenges.*/\1/p' | tail -n 1)

if [ "$PROVISIONED" = 1 ]; then
    printf '\n  %sThis car IS provisioned.%s\n' "$Y" "$X"
    cat <<'EOT'

  Its motor nodes are now STRICT: every command must carry a CRC keyed by that
  node's Data ID, and anything else is ignored in total silence. The raw frames
  below would do nothing and you would learn nothing from that.

  Use the real acceptance path instead:

      sudo ttos-selftest
      python3 ttos-ctf-tool.py walk --salt <fleet salt>     (from your laptop)

  Continuing with checks only; motion steps are skipped.
EOT
    MOTION=0
else
    printf '\n  %sThis car is NOT provisioned.%s\n' "$Y" "$X"
    [ -n "$MISSING" ] && printf '  missing: %s\n' "$MISSING"
    cat <<'EOT'

  It is in FACTORY/TEST mode: open AP "TTOS-TEST", login ttos/ttos, and the
  panel's drive controls are OPEN. Flags and the pivot routine will not work --
  there is no identity to award -- and that is a gate, not a fault.

  This script checks the things the panel cannot: one wheel at a time, so a
  swapped connector cannot hide, plus bus-level evidence.
EOT
fi

# ---------------------------------------------------------------------------
hdr "1. Buses"

for i in "$DRIVE" "$DIAG"; do
    if ! ip link show "$i" >/dev/null 2>&1; then
        no "$i exists" "no such interface -- check the CAN HAT is seated and the overlay loaded"
        continue
    fi
    # The CONTROLLER state, not the link's "state UP" earlier in the same output --
    # matching the wrong one reports UP for a controller that has gone bus-off.
    #
    # An FD interface prints "can <FD> state X" and a classic one "can state X", so
    # the optional token has to be allowed for or every FD bus reads as unknown.
    state=$(ip -details link show "$i" |
            grep -oE 'can (<[A-Z]+> )?state [A-Z-]+' | awk '{print $NF}' | head -n 1)
    case "$state" in
        ERROR-ACTIVE) ok "$i is up and error-active" ;;
        BUS-OFF)      no "$i is BUS-OFF" "nothing else on the bus is ACKing, or a wiring short. Check CAN-H/L and 120R termination." ;;
        ERROR-PASSIVE|ERROR-WARNING)
                      no "$i is $state" "the controller is seeing errors -- suspect termination or a mis-wired node" ;;
        "")           no "$i has no controller state" "is it a real CAN interface?" ;;
        *)            wrn "$i is $state" "expected ERROR-ACTIVE" ;;
    esac
done

# ---------------------------------------------------------------------------
hdr "2. Is anyone home on the drive bus"

# The Pi's own heartbeat. Proves the dashboard is running AND that its transmit
# is being ACKed by at least one other node -- a lone controller on a bus with no
# peers cannot transmit at all, so seeing this at all is a wiring result.
n=$(count_id "$DRIVE" 100 10 3000)
if [ "$n" -ge 10 ]; then
    ok "Pi heartbeat 0x100 seen ($n of 10 in 3 s)"
else
    no "Pi heartbeat 0x100 ($n of 10 in 3 s)" \
       "Either ttos-dashboard is not running, or NOTHING ELSE IS ON THE BUS -- a CAN controller with no peer cannot transmit, because nobody ACKs. Check that at least one node has power."
fi

# The BMS free-runs its telemetry regardless of provisioning, so this is the
# cleanest single proof that the BMS is alive, on the bus, and reading the pack.
n=$(count_id "$DRIVE" 116 10 3000)
if [ "$n" -ge 8 ]; then
    ok "BMS telemetry 0x116 seen ($n of 10 in 3 s)"
    line=$(candump -n 1 -T 2000 "$DRIVE",116:7FF 2>/dev/null || true)
    mv=$(echo "$line" | awk '{print $NF}' >/dev/null 2>&1; echo "$line" |
         sed -n 's/.*\[.\] *\([0-9A-F][0-9A-F]\) *\([0-9A-F][0-9A-F]\).*/\1\2/p')
    if [ -n "$mv" ]; then
        dec=$(printf '%d' "0x$mv" 2>/dev/null || echo 0)
        printf '        pack = %s mV\n' "$dec"
        [ "$dec" -lt 9000 ] && wrn "pack voltage is low ($dec mV)" \
            "the BMS latches a low-voltage cutout and will refuse to close the rail"
    fi
else
    no "BMS telemetry 0x116 ($n of 10 in 3 s)" \
       "BMS not powered, not on the bus, or its CAN pair is swapped. This is the node to check first -- it is the only one that talks unprompted."
fi

# ---------------------------------------------------------------------------
if [ "$MOTION" = 0 ]; then
    hdr "3. Motion -- skipped"
    printf '  (%s)\n' "$([ "$PROVISIONED" = 1 ] && echo "car is provisioned" || echo "--no-motion")"
else

hdr "3. Motion -- WATCH THE ROBOT"

cat <<EOT

  Commands go out as the 6-byte legacy form, which an unprovisioned node accepts.
  ${STEPS} steps at ${RPM} rpm per move. Put the car ON A STAND or give it room.

EOT
ask "Ready to energise the 12 V rail?" || { printf '  stopped.\n'; exit 0; }

cansend "$DRIVE" 115#01
sleep 1
n=$(count_id "$DRIVE" 116 6 2000)
[ "$n" -ge 3 ] && ok "rail command sent, BMS still reporting" \
               || wrn "BMS went quiet after the rail command" "it may have browned out closing the contactor"

if ask "Is the 12 V rail LED on / do the steppers feel energised (they hold position)?"; then
    ok "12 V rail closes"
else
    no "12 V rail did not close" \
       "If the pack is under ~9 V the BMS latches LVC and refuses. Otherwise check the rail MOSFET/relay and its gate line from the BMS."
fi

# Each wheel ALONE, so a swapped pair is visible rather than averaging out into
# "the car moved". This is the fault this section exists to catch.
for w in "LEFT 111" "RIGHT 113"; do
    name=${w% *}; id=${w#* }
    printf '\n  -- %s wheel (0x%s) --\n' "$name" "$id"
    ask "Send $name forward?" || continue
    cansend "$DRIVE" "$id#$(printf '%08X' "$STEPS")01$(printf '%02X' "$RPM")"
    sleep 3
    if ask "Did the $name wheel -- and ONLY the $name wheel -- turn FORWARD?"; then
        ok "$name wheel: correct wheel, correct direction"
    else
        no "$name wheel wrong" \
           "If the OTHER wheel moved, the two motor nodes' CAN connectors are swapped. If it turned BACKWARD, that motor's coil pairs are reversed. If nothing moved, check that node's power and its stepper leads."
    fi
done

printf '\n'
ask "Drop the 12 V rail now?" && { cansend "$DRIVE" 115#02; ok "rail commanded off"; }

fi

# ---------------------------------------------------------------------------
hdr "Summary"
printf '  %s%s passed%s, %s%s failed%s, %s%s warnings%s\n' \
    "$G" "$pass" "$X" "$R" "$fail" "$X" "$Y" "$warn" "$X"

if [ "$PROVISIONED" = 0 ]; then
cat <<'EOT'

  NEXT: this car still has no identity. Until you drop a provisioning file it
  cannot drive from the panel, cannot take a flag, and cannot run the pivot --
  none of which is an assembly fault.

      1. write ttos-provision.conf to the boot partition (exact filename)
      2. boot the car
      3. sudo ttos-selftest
      4. ttos-ctf-tool.py walk --salt <fleet salt>   from your laptop
EOT
fi

[ "$fail" -eq 0 ] || exit 1
exit 0
