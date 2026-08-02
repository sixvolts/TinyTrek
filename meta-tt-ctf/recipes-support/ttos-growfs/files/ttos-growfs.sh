#!/bin/sh
# TTOS CTF first-boot root filesystem expansion.
# Grow the root partition to fill the SD card, then online-resize the ext4 --
# once. Safe by construction: it only ever extends the LAST partition into
# trailing free space (never moves a start, never touches another partition) and
# no-ops on a card that already matches the image. POSIX sh / busybox-ash.
set -u

STATE_DIR=/etc/ttos
MARK="$STATE_DIR/rootfs-expanded"
REBOOT_MARK="$STATE_DIR/rootfs-expand-rebooted"

log() { logger -t ttos-growfs "$*" 2>/dev/null; printf 'ttos-growfs: %s\n' "$*"; }

mkdir -p "$STATE_DIR"
[ -e "$MARK" ] && { log "already expanded; nothing to do"; exit 0; }

# --- Resolve the root partition, its disk, and partition number -------------
ROOT_PART=$(awk '$2=="/"{print $1; exit}' /proc/mounts)
[ "$ROOT_PART" = "/dev/root" ] && ROOT_PART=$(readlink -f /dev/root 2>/dev/null)
case "$ROOT_PART" in
    /dev/*) : ;;
    *) ROOT_PART=$(sed -n 's/.*root=\(\/dev\/[A-Za-z0-9]*\).*/\1/p' /proc/cmdline) ;;
esac
if [ -z "${ROOT_PART:-}" ] || [ ! -b "$ROOT_PART" ]; then
    log "could not resolve root block device (got '${ROOT_PART:-}'); skipping"
    exit 0
fi

name=$(basename "$ROOT_PART")
case "$name" in
    *p[0-9]*) PARTNUM=${name##*p}; DISK=/dev/${name%p*} ;;                                  # mmcblk0p2, nvme0n1p2
    *[0-9])   PARTNUM=$(printf '%s' "$name" | sed 's/^.*[^0-9]//')
              DISK=/dev/$(printf '%s' "$name" | sed 's/[0-9]*$//') ;;                        # sda2
    *) log "unrecognised root device layout '$name'; skipping"; exit 0 ;;
esac
if [ ! -b "$DISK" ]; then log "disk '$DISK' not a block device; skipping"; exit 0; fi
dname=$(basename "$DISK")
log "root=$ROOT_PART disk=$DISK part=$PARTNUM"

# --- Safety gate: only grow if root is the LAST partition on the disk --------
LAST_PART=$(sfdisk -d "$DISK" 2>/dev/null | awk '/^\/dev\//{print $1}' | tail -n 1)
if [ "$LAST_PART" != "$ROOT_PART" ]; then
    log "root is not the last partition (last=$LAST_PART); refusing to grow"
    exit 0
fi

# --- Skip when the card already matches the image (no real free space) -------
disk_sz=$(cat "/sys/class/block/$dname/size" 2>/dev/null || echo 0)
p_start=$(cat "/sys/class/block/$name/start" 2>/dev/null || echo 0)
p_size=$(cat "/sys/class/block/$name/size" 2>/dev/null || echo 0)
free=$(( disk_sz - p_start - p_size ))
if [ "$free" -lt 65536 ]; then                 # < 32 MiB trailing -> nothing worth doing
    log "no significant free space ($free sectors after root); marking done"
    : > "$MARK"; sync
    exit 0
fi

# --- Grow the partition table entry to fill the disk ------------------------
# ', +' keeps the existing start and extends the size over all trailing free
# space. --no-reread: the disk is mounted, so we update the kernel via partx.
if echo ', +' | sfdisk --no-reread -N "$PARTNUM" "$DISK" >/dev/null 2>&1; then
    log "partition $PARTNUM extended to fill disk"
else
    log "sfdisk grow returned non-zero (card may already be full); continuing"
fi
partx -u "$DISK" >/dev/null 2>&1 || true

# --- Confirm the kernel adopted the new size; reboot once if it did not ------
p_size_new=$(cat "/sys/class/block/$name/size" 2>/dev/null || echo 0)
expected=$(( disk_sz - p_start ))
if [ "$p_size_new" -lt $(( expected - 4096 )) ]; then
    if [ ! -e "$REBOOT_MARK" ]; then
        : > "$REBOOT_MARK"; sync
        log "kernel size $p_size_new < expected $expected; rebooting once to re-read the table"
        systemctl reboot
        exit 0
    fi
    log "kernel still stale after one reboot; resizing to the current partition size"
fi

# --- Online-grow the ext4 to fill the partition -----------------------------
if resize2fs "$ROOT_PART" >/dev/null 2>&1; then
    log "ext4 resized on $ROOT_PART"
else
    log "resize2fs found nothing to do (or failed); leaving as-is"
fi

: > "$MARK"; sync
log "rootfs expansion complete"
exit 0
