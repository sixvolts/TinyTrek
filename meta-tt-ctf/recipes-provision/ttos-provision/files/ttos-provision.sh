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
DASH_DEFAULT=/etc/default/ttos-dashboard

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

# set_dash_drive VALUE -- point the dashboard write path at VALUE (a CAN iface,
# e.g. can0, or "" for read-only). Rewrites the DRIVE line in place; the rest of
# the file (operator edits, tuning) is left untouched.
set_dash_drive() {
    [ -f "$DASH_DEFAULT" ] || return 0
    if grep -q '^TTOS_DASH_DRIVE=' "$DASH_DEFAULT"; then
        sed -i "s|^TTOS_DASH_DRIVE=.*|TTOS_DASH_DRIVE=$1|" "$DASH_DEFAULT"
    else
        printf 'TTOS_DASH_DRIVE=%s\n' "$1" >> "$DASH_DEFAULT"
    fi
}

# enter_factory_mode -- reached only when NO provisioning file is present (a
# MALFORMED file still fails loud below). Bring the car up in an UNAUTHENTICATED
# bench-test state so hardware can be exercised before provisioning: an open
# "TTOS-TEST" AP and a live (drivable) dashboard. This is deliberately NOT
# competition-ready and is overwritten the instant a valid provision file applies.
enter_factory_mode() {
    log "no provisioning file -- entering FACTORY/TEST mode (open AP TTOS-TEST, driving enabled)"

    mkdir -p /etc/hostapd
    # Open network: WPA settings omitted, auth_algs=1 (open system auth).
    cat > "$HOSTAPD_CONF" <<'EOF'
# TTOS FACTORY / TEST MODE -- generated, NOT committed source. Open (no WPA) AP
# for bench hardware testing before provisioning. Overwritten on provisioning.
interface=wlan0
driver=nl80211
ssid=TTOS-TEST
country_code=US
ieee80211d=1
hw_mode=a
channel=36
ieee80211n=1
wmm_enabled=1
auth_algs=1
EOF
    chmod 644 "$HOSTAPD_CONF"

    # ttos-ap.service reads country (iw reg) + txpower from here.
    cat > /etc/default/ttos-wifi <<EOF
TTOS_WIFI_COUNTRY=US
TTOS_WIFI_TXPOWER_MBM=500
EOF

    # Let the operator console drive the car for hardware bring-up.
    #
    # can0 is the drive bus: the harness lands on the CAN0 terminal, and the SPI
    # mapping (spi0.0 -> can0, spi1.0 -> can1) is fixed by the device-tree overlay,
    # so this is the same on every car.
    #
    # An earlier note here claimed can1, "verified 2026-08-01". That verification
    # was circular: the Pi transmits its heartbeat on whichever interface is
    # CONFIGURED as the drive bus, and observing it arrive there proves nothing. The
    # bench has no motor nodes or BMS at all -- they are emulated on whichever wire
    # the operator chose. The first real vehicle to run this stack settled it: its
    # BMS beacons 0x116 on can0.
    #
    # Only set it when it is UNSET: factory mode runs on every boot, and blindly
    # rewriting this line would clobber an operator's deliberate choice (it did
    # exactly that, silently, and cost a long debugging session).
    cur_drive=$(sed -n 's/^TTOS_DASH_DRIVE=//p' "$DASH_DEFAULT" 2>/dev/null | head -n 1)
    if [ -z "$cur_drive" ]; then
        set_dash_drive "${TTOS_FACTORY_DRIVE_IF:-can0}"
    else
        log "factory: leaving existing TTOS_DASH_DRIVE=$cur_drive untouched"
    fi

    # Console access for hardware testing: a KNOWN login (ttos/ttos), unlocked, so
    # a blank car is reachable on the serial console. This is a TEST credential --
    # provisioning overwrites it with the per-car secret and re-locks the fleet.
    printf 'ttos:ttos\n' | chpasswd 2>/dev/null
    usermod -U ttos 2>/dev/null

    : > "$STATE_DIR/factory"

    # Make it unmistakable that this car is wide open.
    for tty in /dev/console /dev/tty1 /dev/ttyAMA0; do
        [ -w "$tty" ] && printf '\n\n******************************************************\n*** TTOS FACTORY / TEST MODE\n*** Open AP "TTOS-TEST" (no password), driving ENABLED.\n*** Console login: ttos / ttos\n*** NOT provisioned -- do NOT use for competition.\n******************************************************\n\n' > "$tty" 2>/dev/null
    done
    printf '\n*** TTOS FACTORY / TEST MODE -- open AP "TTOS-TEST", driving ENABLED, login ttos/ttos. ***\n*** This car is NOT provisioned. Provision it to lock down. ***\n\n' > /etc/issue 2>/dev/null

    # Do NOT write the provisioned marker: stay in factory mode until a valid
    # /boot/ttos-provision.conf appears, then provision on the next boot.
    exit 0
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
    # No provisioning data at all -> bench/test state, not a hard failure.
    enter_factory_mode
fi

# --- Parse (no shell eval -- safe for arbitrary PSK characters) -------------
P_HOSTNAME=""; P_CAR_ID=""; P_SSID=""; P_PSK=""; P_COUNTRY=""; P_CHANNEL=""
P_TXPOWER="500"; P_PWHASH=""; P_SSHKEY=""; P_ETH_ADDR=""
P_VIN=""; P_ECU_SERIAL=""; P_FLEET_SALT=""; P_DATAID_L=""; P_DATAID_R=""
P_CODE_C1=""; P_CODE_C2=""; P_CODE_C3=""

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
        # --- CTF challenge data. Validated below, then persisted with the rest of
        # the file into $SRC (mode 600) where the CTF service reads it on every
        # boot. Unlike the WiFi/console values these are NOT applied to any config
        # file here -- they are read at runtime, so they must survive, not be
        # consumed.
        TTOS_VIN)               P_VIN=$val ;;
        TTOS_ECU_SERIAL)        P_ECU_SERIAL=$val ;;
        TTOS_FLEET_SALT)        P_FLEET_SALT=$val ;;
        TTOS_DATAID_L)          P_DATAID_L=$val ;;
        TTOS_DATAID_R)          P_DATAID_R=$val ;;
        TTOS_CODE_C1)           P_CODE_C1=$val ;;
        TTOS_CODE_C2)           P_CODE_C2=$val ;;
        TTOS_CODE_C3)           P_CODE_C3=$val ;;
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

# --- CTF challenge data: fail loud, never default ---------------------------
# A car that boots with an empty VIN answers DID reads with nothing and silently
# breaks C3 -- an unsolvable station that looks perfectly healthy. Refusing to
# provision is the only failure mode anyone will notice in time.
for pair in "TTOS_VIN=$P_VIN" "TTOS_ECU_SERIAL=$P_ECU_SERIAL" \
            "TTOS_FLEET_SALT=$P_FLEET_SALT" \
            "TTOS_DATAID_L=$P_DATAID_L" "TTOS_DATAID_R=$P_DATAID_R" \
            "TTOS_CODE_C1=$P_CODE_C1" "TTOS_CODE_C2=$P_CODE_C2" "TTOS_CODE_C3=$P_CODE_C3"; do
    v=${pair#*=}
    [ -n "$v" ] || fail "missing required challenge key: ${pair%%=*} (regenerate with provisioning/render-fleet.py)"
done

# VIN: 17 chars, no I/O/Q per ISO 3779. The check digit is not verified here --
# the generator computes it and fleet-table.csv is the source of truth.
printf '%s' "$P_VIN" | grep -qE '^[A-HJ-NPR-Z0-9]{17}$' \
    || fail "TTOS_VIN must be 17 chars, no I/O/Q (got '$P_VIN')"

# CRC Data IDs: 0x0001..0xFFFE, and the two motors must differ -- identical Data
# IDs would let a contestant forge one wheel's frames from the other's.
for pair in "TTOS_DATAID_L=$P_DATAID_L" "TTOS_DATAID_R=$P_DATAID_R"; do
    v=${pair#*=}
    printf '%s' "$v" | grep -qiE '^0x[0-9a-f]{4}$' \
        || fail "${pair%%=*} must be 0xNNNN (got '$v')"
    if printf '%s' "$v" | grep -qiE '^0x(0000|ffff)$'; then
        fail "${pair%%=*} must be 0x0001..0xFFFE (got '$v')"
    fi
done
[ "$P_DATAID_L" != "$P_DATAID_R" ] || fail "TTOS_DATAID_L and TTOS_DATAID_R must differ"

# Unlock codes: 8 chars, first char is the challenge number, remainder from the
# retype-safe alphabet (no 0/O/1/I/L). These get read off a screen and typed back.
for pair in "TTOS_CODE_C1=1:$P_CODE_C1" "TTOS_CODE_C2=2:$P_CODE_C2" "TTOS_CODE_C3=3:$P_CODE_C3"; do
    n=${pair#*=}; n=${n%%:*}
    v=${pair#*:}
    printf '%s' "$v" | grep -qE "^${n}[23456789ABCDEFGHJKMNPQRSTVWXYZ]{7}$" \
        || fail "${pair%%=*} must be '${n}' + 7 chars of 23456789ABCDEFGHJKMNPQRSTVWXYZ (got '$v')"
done

# Fleet salt: 32 hex chars (16 random bytes). Not secret -- it ships in the client
# JS -- but a malformed one silently changes every derived service key.
printf '%s' "$P_FLEET_SALT" | grep -qiE '^[0-9a-f]{32}$' \
    || fail "TTOS_FLEET_SALT must be 32 hex chars"

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
    # mkdir + chmod, NOT `install -d`. THERE IS NO install(1) ON THIS ROOTFS --
    # busybox is built without it. `install -d` fails, the directory is never
    # created, the redirect below then fails too, and the key is silently not
    # deployed. Nothing else in provisioning depends on it, so the run continues
    # and reports success; the only symptom is that key auth does not work,
    # discovered whenever someone next tries to log in without a password.
    # Found on the first real provisioning run, 2026-08-03.
    mkdir -p /home/ttos/.ssh || fail "could not create /home/ttos/.ssh"
    chmod 700 /home/ttos/.ssh
    printf '%s\n' "$P_SSHKEY" > /home/ttos/.ssh/authorized_keys \
        || fail "could not write /home/ttos/.ssh/authorized_keys"
    chmod 600 /home/ttos/.ssh/authorized_keys
    chown -R ttos:ttos /home/ttos/.ssh
    log "installed authorized_keys for ttos"
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

# Lock down: undo any prior FACTORY/TEST state. The open hostapd.conf was already
# overwritten above with the WPA2 config; here we return the dashboard to its
# read-only safety gate and drop the factory marker so it can't be mistaken for
# (or reverted to) test mode.
set_dash_drive ""
rm -f "$STATE_DIR/factory"

# --- Consume: stage into the rootfs, THEN wipe the FAT copy -----------------
# Only now that everything validated and applied. A failed run above leaves the
# FAT file intact so it can be corrected in the field (§5.6).
if [ "$SRCFILE" != "$SRC" ]; then
    cp "$SRCFILE" "$SRC"
    # 640 root:ttos-secrets, NOT 600 root.
    #
    # ttos-dashboard runs with DynamicUser=yes -- a transient unprivileged user --
    # so a 600 root file is unreadable to it and the CTF layer logs "provision.src
    # unreadable: permission denied" and serves NO DIDs and NO unlock codes. Every
    # challenge on the car is then dead, and it looks identical to the unprovisioned
    # case, which is why this survived: every hardware run so far was in factory
    # mode, where the file legitimately does not exist. Caught on the bench
    # 2026-08-03, the first time a provisioned identity met the Phase 1 CTF layer.
    #
    # The unit carries SupplementaryGroups=ttos-secrets; that group is the only
    # thing that can read this file besides root.
    chown root:ttos-secrets "$SRC" 2>/dev/null || chown root:root "$SRC"
    chmod 640 "$SRC"
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
