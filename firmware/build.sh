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
  arduino-cli compile --fqbn "$FQBN" --libraries "$HERE/libraries" "${EXTRA_FLAGS[@]}" "$HERE/$s"
  if [ -n "$PORT" ]; then
    echo "== uploading $s -> $PORT =="
    arduino-cli upload -p "$PORT" --fqbn "$FQBN" --libraries "$HERE/libraries" "$HERE/$s"
  fi
done
