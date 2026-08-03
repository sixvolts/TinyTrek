#!/usr/bin/env bash
# Batch-build the node firmware from a terminal. OPTIONAL -- the normal way to
# flash a node is to open its .ino in the Arduino IDE, pick the board, and upload.
# Each sketch folder carries its own library sources, so the IDE needs no setup.
#
# This exists for building all three at once without clicking through the IDE.
# Requires arduino-cli.
#
#   ./build.sh <fqbn> [sketch]
#     ./build.sh arduino:avr:uno                 # build all three
#     ./build.sh arduino:avr:uno TinytrekBMS     # build one
#     ./build.sh arduino:avr:uno TinytrekBMS <port>   # build + upload
#
# The FQBN must match your node board (e.g. arduino:avr:uno). The key bit is
#, which points the compiler at firmware/libraries/CAN
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

# NO BUILD-TIME CONSTANTS, AND NO SECRETS IN THIS TREE.
#
# The nodes get their Data IDs, unlock codes and detector thresholds from the Pi
# over CAN during provisioning, and store them in flash. So there is exactly ONE
# binary per node type for the entire fleet, this build takes no car id, and nothing
# here needs provisioning/ to be present.
#
# A node with nothing stored stays permissive and the car drives normally, which is
# what makes an unprovisioned car usable during setup.
EXTRA_FLAGS=()

for s in $SKETCHES; do
  echo "== compiling $s ($FQBN) =="
  arduino-cli compile --fqbn "$FQBN" "${EXTRA_FLAGS[@]}" "$HERE/$s"
  if [ -n "$PORT" ]; then
    echo "== uploading $s -> $PORT =="
    arduino-cli upload -p "$PORT" --fqbn "$FQBN" "$HERE/$s"
  fi
done
