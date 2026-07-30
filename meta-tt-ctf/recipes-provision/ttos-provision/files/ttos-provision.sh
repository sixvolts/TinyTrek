#!/bin/sh
# TTOS CTF first-boot provisioning (§5.6).
# One image for the whole fleet; per-car values applied here on first boot from a
# plain-text file on the FAT boot partition. POSIX sh / busybox-ash compatible.
set -u

STATE_DIR=/etc/ttos
MARKER="$STATE_DIR/provisioned"
SRC="$STATE_DIR/provision.src"          # secrets moved into the rootfs (600 root)
LOG="$STATE_DIR/provision.log"
TEMPLATE=/etc/hostapd/hostapd.conf.template
HOSTAPD_CONF=/etc/hostapd/hostapd.conf

# Candidate locations of the provisioning file on the FAT boot partition.
FAT_CANDIDATES="/boot/firmware/ttos-provision.conf /boot/ttos-provision.conf"

log() { logger -t ttos-provision "$*" 2>/dev/null; printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)" "$*" >> "$LOG" 2>/dev/null; }

fail() {
    msg="TTOS PROVISIONING FAILED: $*"
    logger -t ttos-provision "$msg" 2>/dev/null
    # Shout on every console a human might be watching.
    for tty in /dev/console /dev/tty1 /dev/ttyAMA0; do
        [ -w "$tty" ] && printf '\n\n******************************************************\n*** %s\n*** Car is UNPROVISIONED. Fix /boot/ttos-provision.conf and reboot.\n******************************************************\n\n' "$msg" > "$tty" 2>/dev/null
    done
    # Persistent warning at the login prompt.
    printf '\n*** %s ***\n*** This car is NOT provisioned -- do not use for competition. ***\n\n' "$msg" > /etc/issue 2>/dev/null
    exit 1
}

# --- Idempotency ------------------------------------------------------------
if [ -e "$MARKER" ]; then
    log "already provisioned; nothing to do"
    exit 0
fi

mkdir -p "$STATE_DIR"

# --- Locate the source ------------------------------------------------------
# IMPORTANT: do NOT consume (wipe) the FAT file until AFTER validation+apply
# succeed. A malformed file must stay on the FAT partition so it can be fixed in
# the field with a laptop (§5.6). The work copy lives on tmpfs (/run), cleared on
# reboot, so nothing sensitive is persisted on a failed run.
# Wait up to ~20s for the boot partition to mount and the file to appear (guards
# against a first-boot mount race). Bail early if a staged rootfs copy exists.
FATFILE=""
tries=0
while [ "$tries" -lt 20 ]; do
    for c in $FAT_CANDIDATES; do
        if [ -f "$c" ]; then FATFILE="$c"; break; fi
    done
    [ -n "$FATFILE" ] && break
    [ -f "$SRC" ] && break
    tries=$((tries + 1))
    sleep 1
done

WORK=/run/ttos-provision.work
if [ -n "$FATFILE" ]; then
    # Normalise Windows/macOS CRLF into a tmpfs work copy; leave the FAT file be.
    tr -d '\r' < "$FATFILE" > "$WORK" || fail "cannot read $FATFILE"
    chmod 600 "$WORK"
    SRCFILE="$WORK"
    log "found provisioning file: $FATFILE"
elif [ -f "$SRC" ]; then
    # Crash-recovery: a prior run validated + staged to the rootfs but did not finish.
    SRCFILE="$SRC"
    log "resuming from staged provisioning data (FAT copy already consumed)"
else
    fail "no provisioning file found (looked for: $FAT_CANDIDATES)"
fi

# --- Parse (no shell eval -- safe for arbitrary PSK characters) -------------
P_HOSTNAME=""; P_CAR_ID=""; P_SSID=""; P_PSK=""; P_COUNTRY=""; P_CHANNEL=""
P_TXPOWER="500"; P_PWHASH=""; P_SSHKEY=""; P_ETH_ADDR=""

# Strip a trailing inline comment (whitespace + # ... to EOL), then surrounding
# whitespace and one layer of quotes. A '#' NOT preceded by whitespace is kept,
# so a PSK may contain '#'. Avoid a literal " #" inside a value.
trim() { printf '%s' "$1" | sed 's/[[:space:]]#.*$//; s/^[[:space:]]*//; s/[[:space:]]*$//; s/^"//; s/"$//'; }

while IFS='=' read -r key val; do
    case "$key" in ''|\#*) continue ;; esac
    key=$(printf '%s' "$key" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
    val=$(trim "$val")
    case "$key" in
        TTOS_HOSTNAME)          P_HOSTNAME=$val ;;
        TTOS_CAR_ID)            P_CAR_ID=$val ;;
        TTOS_WIFI_SSID)         P_SSID=$val ;;
        TTOS_WIFI_PSK)          P_PSK=$val ;;
        TTOS_WIFI_COUNTRY)      P_COUNTRY=$val ;;
        TTOS_WIFI_CHANNEL)      P_CHANNEL=$val ;;
        TTOS_WIFI_TXPOWER_MBM)  P_TXPOWER=$val ;;
        TTOS_CONSOLE_PW_HASH)   P_PWHASH=$val ;;
        TTOS_SSH_AUTHORIZED_KEY) P_SSHKEY=$val ;;
        TTOS_ETH_ADDRESS)       P_ETH_ADDR=$val ;;
        *) log "ignoring unknown key: $key" ;;
    esac
done < "$SRCFILE"

# --- Validate (fail loud, never apply defaults) -----------------------------
for pair in "TTOS_HOSTNAME=$P_HOSTNAME" "TTOS_CAR_ID=$P_CAR_ID" \
            "TTOS_WIFI_SSID=$P_SSID" "TTOS_WIFI_PSK=$P_PSK" \
            "TTOS_WIFI_COUNTRY=$P_COUNTRY" "TTOS_WIFI_CHANNEL=$P_CHANNEL" \
            "TTOS_CONSOLE_PW_HASH=$P_PWHASH"; do
    v=${pair#*=}
    [ -n "$v" ] || fail "missing required key: ${pair%%=*}"
done

# Console password MUST be a crypt hash, never plaintext (§5.5).
case "$P_PWHASH" in
    \$6\$*|\$y\$*|\$2b\$*|\$2y\$*|\$5\$*|\$7\$*) : ;;
    *) fail "TTOS_CONSOLE_PW_HASH is not a crypt hash (expected \$y\$/\$6\$...); refusing plaintext" ;;
esac

# WPA2 PSK: either an 8..63-char passphrase, or a 64 hex-digit raw PSK (PMK).
# Anything else (e.g. a crypt hash pasted by mistake) fails loud.
plen=$(printf '%s' "$P_PSK" | wc -c)
if printf '%s' "$P_PSK" | grep -qiE '^[0-9a-f]{64}$'; then
    PSK_HEX=1
elif [ "$plen" -ge 8 ] && [ "$plen" -le 63 ]; then
    PSK_HEX=0
else
    fail "TTOS_WIFI_PSK must be an 8..63 char passphrase or 64 hex digits (got $plen chars)"
fi

# Non-DFS 5 GHz channel guard (US set); warn but do not hard-fail on others.
case " 36 40 44 48 149 153 157 161 " in
    *" $P_CHANNEL "*) : ;;
    *) log "WARNING: channel $P_CHANNEL is not in the US non-DFS set (36/40/44/48/149/153/157/161)" ;;
esac

# --- Apply ------------------------------------------------------------------
printf '%s\n' "$P_HOSTNAME" > /etc/hostname
hostname "$P_HOSTNAME" 2>/dev/null
printf '%s\n' "$P_CAR_ID" > "$STATE_DIR/car-id"

# hostapd.conf written directly (heredoc, no re-parsing) -- safe for any PSK chars.
mkdir -p /etc/hostapd
cat > "$HOSTAPD_CONF" <<EOF
interface=wlan0
driver=nl80211
ssid=$P_SSID
country_code=$P_COUNTRY
ieee80211d=1
hw_mode=a
channel=$P_CHANNEL
ieee80211n=1
wmm_enabled=1
auth_algs=1
wpa=2
wpa_key_mgmt=WPA-PSK
rsn_pairwise=CCMP
EOF
if [ "$PSK_HEX" = 1 ]; then
    echo "wpa_psk=$P_PSK" >> "$HOSTAPD_CONF"          # raw 256-bit PMK (64 hex digits)
else
    echo "wpa_passphrase=$P_PSK" >> "$HOSTAPD_CONF"   # 8..63 char passphrase
fi
chmod 600 "$HOSTAPD_CONF"

cat > /etc/default/ttos-wifi <<EOF
TTOS_WIFI_COUNTRY=$P_COUNTRY
TTOS_WIFI_TXPOWER_MBM=$P_TXPOWER
EOF

# Console/SSH account: set the 'ttos' user's password from the hash and unlock it.
# root stays locked (set at image build); ops log in as ttos and sudo (§5.5).
printf 'ttos:%s\n' "$P_PWHASH" | chpasswd -e || fail "chpasswd failed for ttos"
usermod -U ttos 2>/dev/null

if [ -n "$P_SSHKEY" ]; then
    install -d -m 700 /home/ttos/.ssh
    printf '%s\n' "$P_SSHKEY" > /home/ttos/.ssh/authorized_keys
    chmod 600 /home/ttos/.ssh/authorized_keys
    chown -R ttos:ttos /home/ttos/.ssh
fi

if [ -n "$P_ETH_ADDR" ]; then
    mkdir -p /etc/systemd/network/20-wired.network.d
    cat > /etc/systemd/network/20-wired.network.d/10-static.conf <<EOF
[Network]
Address=$P_ETH_ADDR
EOF
fi

# Per-car SSH host keys (§5.6: otherwise the whole fleet ships identical keys).
rm -f /etc/ssh/ssh_host_*
ssh-keygen -A >/dev/null 2>&1 || fail "ssh-keygen -A failed"

# Restore a clean login banner (in case a previous failed run left a warning).
printf 'TinyTrekOS-CTF \\n \\l\n\n' > /etc/issue 2>/dev/null

# --- Consume: stage into the rootfs, THEN wipe the FAT copy -----------------
# Only now that everything validated and applied. A failed run above leaves the
# FAT file intact so it can be corrected in the field (§5.6).
if [ "$SRCFILE" != "$SRC" ]; then
    cp "$SRCFILE" "$SRC" && chmod 600 "$SRC"
fi
if [ -n "${FATFILE:-}" ] && [ -f "$FATFILE" ]; then
    sz=$(wc -c < "$FATFILE" 2>/dev/null || echo 0)
    head -c "$sz" /dev/urandom > "$FATFILE" 2>/dev/null
    sync; rm -f "$FATFILE"; sync
    log "consumed provisioning file from $FATFILE"
fi
rm -f "$WORK" 2>/dev/null

# --- Finalize ---------------------------------------------------------------
: > "$MARKER"
sync
# Non-secret audit line so a judge can confirm which car this is.
log "provisioned OK: car_id=$P_CAR_ID hostname=$P_HOSTNAME ssid=$P_SSID channel=$P_CHANNEL country=$P_COUNTRY txpower_mbm=$P_TXPOWER"
exit 0
