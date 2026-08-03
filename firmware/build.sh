#!/usr/bin/env bash
# Build the TinyTrek node firmware against the VENDORED CAN library (no global
# library install needed). Requires arduino-cli.
#
#   ./build.sh <fqbn> [sketch]
#     ./build.sh arduino:avr:uno                 # build all three
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

# ---- per-car challenge constants -------------------------------------------
# TTOS_CAR sets which car is being flashed. The Data IDs and the C2/C3 codes are
# SECRETS: they live in provisioning/firmware-constants.h, which is gitignored, and
# are staged into each sketch directory as ttos-fleet.h at build time. The staged
# copies are gitignored too -- never commit one, and never leave one on a machine
# that is not doing the flashing.
#
# Arduino only compiles headers that sit in the sketch directory, which is why this
# copies rather than adding an include path.
CAR="${TTOS_CAR:-}"
FLEET_SRC="$HERE/../provisioning/firmware-constants.h"
if [ -n "$CAR" ]; then
  [ -f "$FLEET_SRC" ] || { echo "TTOS_CAR=$CAR but $FLEET_SRC is missing" >&2; exit 2; }
  case "$CAR" in [0-9][0-9]) ;; *) echo "TTOS_CAR must be two digits (01..08), got '$CAR'" >&2; exit 2;; esac
  grep -q "TTOS_CAR_$CAR" "$FLEET_SRC" || { echo "no constants for car $CAR in $FLEET_SRC" >&2; exit 2; }
  echo "== staging challenge constants for car $CAR =="
  for s in $SKETCHES; do cp "$FLEET_SRC" "$HERE/$s/ttos-fleet.h"; done
  # An ARRAY, not a string: the property value contains a space, and word-splitting
  # an unquoted string hands arduino-cli "-DTTOS_CHALLENGE=1" as its own argument,
  # which it rejects as an unknown flag.
  EXTRA_FLAGS=(--build-property "compiler.cpp.extra_flags=-DTTOS_CAR_$CAR -DTTOS_CHALLENGE=1")
else
  # No car selected: build the BASELINE firmware. Protection and detection compile
  # out entirely, so an un-staged tree still builds and still drives -- which is
  # what you want on the bench and for anyone debugging the drivetrain.
  echo "== TTOS_CAR unset: building BASELINE firmware (no CRC checking, no detection) =="
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
