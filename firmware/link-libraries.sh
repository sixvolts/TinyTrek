#!/usr/bin/env bash
# Make the vendored libraries visible to the Arduino IDE.
#
#     ./link-libraries.sh
#
# build.sh does not need this -- it passes --libraries at the vendored tree, which
# is why the libraries are vendored in the first place: no global installs, and
# everyone builds against the same versions.
#
# The IDE has no equivalent. It only looks in your sketchbook, so if you are
# flashing from the IDE the vendored copies have to be linked in. Symlinks, not
# copies, so the tree stays the single source and an update here is picked up.
#
# Three libraries, and all three are load-bearing:
#   CAN                 the MCP2515/MCP2518FD driver
#   Adafruit_NeoPixel   the onboard status light
#   FlashStorage        SAMD21 flash-as-EEPROM, where a motor node stores the
#                       Data ID it is provisioned with. The RP2040 core has its
#                       own EEPROM emulation and does not need this.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for d in "$HOME/Documents/Arduino/libraries" "$HOME/Arduino/libraries"; do
    [ -d "$(dirname "$d")" ] && SKETCHBOOK="$d" && break
done
: "${SKETCHBOOK:=$HOME/Documents/Arduino/libraries}"
mkdir -p "$SKETCHBOOK"

for lib in "$HERE"/libraries/*/; do
    name=$(basename "$lib")
    target="$SKETCHBOOK/$name"
    if [ -L "$target" ]; then
        rm -f "$target"
    elif [ -e "$target" ]; then
        printf '  %-20s SKIPPED -- a real directory is already there\n' "$name"
        printf '  %-20s move or delete %s first\n' "" "$target"
        continue
    fi
    ln -s "${lib%/}" "$target"
    printf '  %-20s linked -> %s\n' "$name" "$target"
done

printf '\nRestart the Arduino IDE so it rescans the sketchbook.\n'
