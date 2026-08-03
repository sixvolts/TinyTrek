#!/usr/bin/env bash
# Build the TinyTrek node firmware against the VENDORED CAN library (no global
# library install needed). Requires arduino-cli.
#
#   ./build.sh <fqbn> [sketch]
#     ./build.sh arduino:avr:uno                    # baseline, all three
#     TTOS_CHALLENGE=1 ./build.sh arduino:avr:uno   # challenge firmware
#     ./build.sh arduino:avr:uno TinytrekBMS     # build one
#     ./build.sh arduino:avr:uno TinytrekBMS <port>   # build + upload
#
# The FQBN must match your node board (e.g. arduino:avr:uno). The key bit is
# --libraries "$HERE/libraries", which points the compiler at firmware/libraries/CAN
# instead of ~/Documents/Arduino/libraries.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

FQBN="${1:-${TINYTREK_FQBN:-}}"
if [ -z "$FQBN" ]; then
  echo "usage: $0 <fqbn> [sketch] [port]   (e.g. $0 arduino:avr:uno)" >&2
  exit 2
fi

if [ -n "${2:-}" ]; then SKETCHES="$2"; else SKETCHES="TinytrekLMotor TinytrekRMotor TinytrekBMS"; fi
PORT="${3:-}"

# ---- challenge constants ---------------------------------------------------
# TTOS_CHALLENGE=1 builds challenge firmware; unset builds baseline.
#
# FLEET-WIDE, NOT PER-CAR (changed 2026-08-03). THREE binaries for the whole fleet
# -- left motor, right motor, BMS -- so any node is a drop-in for the same position
# on any car, and a car that dies mid-challenge can be swapped without a team
# losing the work they have done. Per-car identity is all Pi-side and provisioned:
# hostname, WiFi, VIN, ECU serial, console password.
#
# The constants are still SECRETS -- the Data IDs are what Challenge 3 recovers --
# so they live in provisioning/firmware-constants.h, which is gitignored, and are
# staged into each sketch directory as ttos-fleet.h at build time. Arduino only
# compiles headers sitting in the sketch directory, which is why this copies rather
# than adding an include path. The staged copies are gitignored too; delete them
# when you are done:   rm -f firmware/*/ttos-fleet.h
FLEET_SRC="$HERE/../provisioning/firmware-constants.h"
if [ -n "${TTOS_CHALLENGE:-}" ]; then
  [ -f "$FLEET_SRC" ] || { echo "TTOS_CHALLENGE set but $FLEET_SRC is missing" >&2; exit 2; }
  echo "== staging challenge constants (fleet-wide) =="
  for sk in $SKETCHES; do cp "$FLEET_SRC" "$HERE/$sk/ttos-fleet.h"; done
  # An ARRAY, not a string: the property value contains a space, and word-splitting
  # an unquoted string hands arduino-cli a stray argument it rejects as a flag.
  EXTRA_FLAGS=(--build-property "compiler.cpp.extra_flags=-DTTOS_CHALLENGE=1")
else
  echo "== TTOS_CHALLENGE unset: building BASELINE firmware (no CRC checking, no detection) =="
  EXTRA_FLAGS=()
fi

for s in $SKETCHES; do
  echo "== compiling $s ($FQBN) =="
  arduino-cli compile --fqbn "$FQBN" --libraries "$HERE/libraries" "${EXTRA_FLAGS[@]}" "$HERE/$s"
  if [ -n "$PORT" ]; then
    echo "== uploading $s -> $PORT =="
    arduino-cli upload -p "$PORT" --fqbn "$FQBN" --libraries "$HERE/libraries" "$HERE/$s"
  fi
done
