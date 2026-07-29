#!/usr/bin/env bash
# Build the TTOS CTF image in the pinned container (§6).
#
# Usage:
#   ./build.sh                      # build production image (kas/ttos-ctf.yml)
#   ./build.sh kas/ttos-ctf-dev.yml # build bench image
#   ./build.sh --build [kas-file]   # force-rebuild the container first
#   TTOS_YOCTO_DIR=/path ./build.sh # override where TMPDIR/DL_DIR/SSTATE live
#
# No SSH agent is forwarded and no private hosts are keyscanned (§4.3): the
# baseline builds from a fresh clone with no private credentials.
set -euo pipefail

IMAGE="ttos-ctf-build:latest"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Heavy build dirs on persistent storage OUTSIDE the build tree (§6).
# NOTE: on 'arcana' as briefed this should be the Optane P5800X; on the current
# host there is a single NVMe, so this defaults under $HOME. Point it at the
# fastest scratch you have and keep it off NFS / spinning disk.
YOCTO_DIR="${TTOS_YOCTO_DIR:-$HOME/yocto}"
mkdir -p "$YOCTO_DIR"/downloads "$YOCTO_DIR"/sstate-cache "$YOCTO_DIR"/tmp

# (Re)build the container image if missing or if --build was requested.
if [[ "${1:-}" == "--build" ]] || ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    docker build \
        --build-arg HOST_UID="${SUDO_UID:-$(id -u)}" \
        --build-arg HOST_GID="${SUDO_GID:-$(id -g)}" \
        -t "$IMAGE" "$REPO_DIR/docker"
    [[ "${1:-}" == "--build" ]] && shift || true
fi

KAS_FILE="${1:-kas/ttos-ctf.yml}"

exec docker run --rm -it \
    -v "$REPO_DIR":"$REPO_DIR" \
    -v "$YOCTO_DIR":/yocto \
    --workdir "$REPO_DIR" \
    "$IMAGE" \
    kas build "$KAS_FILE"
