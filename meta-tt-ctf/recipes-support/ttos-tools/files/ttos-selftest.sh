#!/bin/sh
# ttos-selftest.sh -- verify the TinyTrekOS CTF baseline on real hardware.
# Maps to the acceptance criteria in the build brief (§7). POSIX sh / busybox-ash.
#
# Run ON THE PI:
#     sudo sh ttos-selftest.sh            # non-destructive checks only
#     sudo sh ttos-selftest.sh --loopback # also runs a CAN loopback self-test that
#                                          # temporarily reconfigures can0/can1
#                                          # (a reboot restores normal config)
#
# Exit code 0 = no failures. SKIP means "needs a second machine / manual check".

LOOPBACK=0
[ "${1:-}" = "--loopback" ] && LOOPBACK=1

PASS=0; FAIL=0; SKIP=0
if [ -t 1 ]; then G='\033[32m'; R='\033[31m'; Y='\033[33m'; B='\033[1m'; N='\033[0m'; else G=; R=; Y=; B=; N=; fi
ok(){ printf "  ${G}[PASS]${N} %s\n" "$1"; PASS=$((PASS+1)); }
no(){ printf "  ${R}[FAIL]${N} %s\n" "$1"; FAIL=$((FAIL+1)); }
sk(){ printf "  ${Y}[SKIP]${N} %s\n" "$1"; SKIP=$((SKIP+1)); }
info(){ printf "         %s\n" "$1"; }
hdr(){ printf "\n${B}== %s ==${N}\n" "$1"; }

SUDO=""; [ "$(id -u)" != 0 ] && SUDO="sudo"
have(){ command -v "$1" >/dev/null 2>&1; }

printf "${B}TinyTrekOS-CTF self-test${N}  (%s)\n" "$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)"

# ---------------------------------------------------------------------------
hdr "1. Boot, identity, memory  (§7: boots, 2GB headroom, provisioned identity)"
VARIANT=$(cat /etc/ttos-variant 2>/dev/null || echo "unknown")
info "variant: $VARIANT"
case "$VARIANT" in
    *production*) ok "running the PRODUCTION image" ;;
    *BENCH*|*bench*) sk "running the BENCH image (debug-tweaks) -- security checks below will be lax by design" ;;
    *) no "no /etc/ttos-variant marker (unexpected image)" ;;
esac

HN=$(hostname)
info "hostname: $HN"
if [ "$HN" = "ttos-ctf-unprovisioned" ]; then
    no "hostname is still 'ttos-ctf-unprovisioned' -- provisioning did NOT run"
else
    ok "hostname set by provisioning: $HN"
fi

if have free; then
    AVAIL_KB=$(free -k 2>/dev/null | awk '/^Mem:/{print $7}')
    AVAIL_MB=$(( ${AVAIL_KB:-0} / 1024 ))
    info "MemAvailable: ${AVAIL_MB} MiB   ($(free -h 2>/dev/null | awk '/^Mem:/{print $2" total"}'))"
    if [ "${AVAIL_MB:-0}" -ge 200 ]; then ok "memory headroom looks sane (>=200 MiB available)"
    else no "low memory headroom (${AVAIL_MB} MiB available)"; fi
else sk "free(1) not available"; fi
info "uptime:$(uptime 2>/dev/null | sed 's/^.*up/ up/; s/,.*load/  load/')"

# ---------------------------------------------------------------------------
hdr "2. Kernel CAN config  (§4.1 go/no-go: CAN_GW + MCP251XFD compiled in)"
if [ -r /proc/config.gz ] && have zcat; then
    CFG=$(zcat /proc/config.gz 2>/dev/null)
    echo "$CFG" | grep -q '^CONFIG_CAN_GW=[ym]' && ok "CONFIG_CAN_GW enabled" || no "CONFIG_CAN_GW NOT enabled"
    echo "$CFG" | grep -q '^CONFIG_CAN_MCP251XFD=[ym]' && ok "CONFIG_CAN_MCP251XFD enabled" || no "CONFIG_CAN_MCP251XFD NOT enabled"
    echo "$CFG" | grep -q '^CONFIG_CAN_RAW=[ym]' && ok "CONFIG_CAN_RAW enabled" || no "CONFIG_CAN_RAW NOT enabled"
else
    sk "/proc/config.gz not readable (CONFIG_IKCONFIG_PROC off) -- check with: modinfo mcp251xfd; ls /sys/module/can_gw"
fi

# ---------------------------------------------------------------------------
hdr "3. CAN controllers probing  (§7: dmesg shows both controllers)"
DM=$($SUDO dmesg 2>/dev/null | grep -i mcp251xfd)
NPROBE=$(printf '%s\n' "$DM" | grep -ci 'MCP2518FD\|successfully initialized\|rev')
if [ "$NPROBE" -ge 2 ]; then ok "mcp251xfd: 2 controllers probed"
elif [ "$NPROBE" -eq 1 ]; then no "mcp251xfd: only 1 controller probed (expected 2 -- check HAT Mode A jumpers/wiring)"
else no "mcp251xfd: no controllers probed (check HAT seating, overlays, SPI)"; fi
[ -n "$DM" ] && printf '%s\n' "$DM" | tail -n 4 | sed 's/^/         /'

# ---------------------------------------------------------------------------
# Both buses run CAN FD at 500k arbitration / 1 Mbit data since CTF phase 1: can0
# (DIAG) was switched from classic so a routine can return a flag in one 64-byte
# response with no ISO-TP. Role names, not numbers -- DRIVE is the HIGHER-numbered
# interface here (can1), which is the opposite of the original design doc.
hdr "4. CAN interfaces  (DRIVE=can1 and DIAG=can0, both FD 500k/1M)"
check_can(){ # $1=iface  $2=expect_fd(0/1)
    ifc=$1; fd=$2
    if ! ip link show "$ifc" >/dev/null 2>&1; then no "$ifc does not exist"; return; fi
    # networkd may still be configuring right after boot; wait for the admin UP flag.
    # NOTE: CAN interfaces report operstate UNKNOWN even when up, so we check the
    # <...,UP,...> admin flag, not 'state UP'.
    i=0; while [ "$i" -lt 8 ]; do ip link show "$ifc" 2>/dev/null | head -n 1 | grep -qE '[<,]UP[,>]' && break; i=$((i+1)); sleep 1; done
    D=$(ip -details -statistics link show "$ifc" 2>/dev/null)
    STATE=$(echo "$D" | grep -o 'can state [A-Z-]*' | awk '{print $3}')
    BR=$(echo "$D" | grep -o 'bitrate [0-9]*' | head -n 1 | awk '{print $2}')
    if ip link show "$ifc" 2>/dev/null | head -n 1 | grep -qE '[<,]UP[,>]'; then ok "$ifc is UP"
    else no "$ifc is DOWN (should come up automatically at boot)"; fi
    [ "$BR" = "500000" ] && ok "$ifc arbitration bitrate = 500000" || no "$ifc bitrate = ${BR:-?} (expected 500000)"
    if [ "$fd" = "1" ]; then
        DBR=$(echo "$D" | grep -o 'dbitrate [0-9]*' | awk '{print $2}')
        echo "$D" | grep -qi '\<fd\>\|FD' && ok "$ifc is CAN FD" || no "$ifc is not in FD mode"
        [ "$DBR" = "1000000" ] && ok "$ifc data bitrate = 1000000" || no "$ifc dbitrate = ${DBR:-?} (expected 1000000)"
    fi
    [ "$STATE" = "ERROR-ACTIVE" ] && ok "$ifc state ERROR-ACTIVE" || sk "$ifc state = ${STATE:-?} (ERROR-ACTIVE needs a wired, terminated bus)"
}
check_can can0 1
check_can can1 1

# ---------------------------------------------------------------------------
hdr "5. cangw gateway  (§4.1 go/no-go: cangw -L works, a rule takes effect)"
if have cangw; then
    # Test cangw -L by whether it PRODUCES A LISTING, not by exit status: it returns
    # non-zero even on success, which reported a working gateway as broken on every
    # car. Confirmed 2026-08-02 -- section 12 counted 2 live rules from this same
    # command while this check called it a failure.
    if $SUDO cangw -L 2>/dev/null | grep -q 'cangw' || $SUDO cangw -L >/dev/null 2>&1; then
        ok "cangw -L runs"
        if ip link show can0 >/dev/null 2>&1 && ip link show can1 >/dev/null 2>&1; then
            $SUDO cangw -A -s can0 -d can1 -e >/dev/null 2>&1
            if $SUDO cangw -L 2>/dev/null | grep -q 'can0.*can1'; then
                ok "cangw forwarding rule can0->can1 installed"
            else no "cangw rule did not take effect"; fi
            $SUDO cangw -F >/dev/null 2>&1  # flush test rule
            info "test rule flushed"
        else sk "can0/can1 not both present -- skipping live rule test"; fi
    else no "cangw -L failed (is CONFIG_CAN_GW in the running kernel?)"; fi
else no "cangw binary missing (can-utils not installed?)"; fi

# ---------------------------------------------------------------------------
hdr "6. CAN stack + tools smoke test (vcan loopback -- non-destructive)"
if have cansend && have candump; then
    $SUDO modprobe vcan 2>/dev/null
    if $SUDO ip link add dev vcanTEST type vcan 2>/dev/null && $SUDO ip link set vcanTEST up 2>/dev/null; then
        VF=$(mktemp 2>/dev/null || echo /tmp/vcan.$$)
        $SUDO candump -n 1 vcanTEST > "$VF" 2>/dev/null &
        CPID=$!
        # INTEGER sleeps only. busybox sleep does not necessarily support fractional
        # seconds, and when it does not it returns immediately -- so cansend fired
        # before candump had bound, the frame went nowhere, and a perfectly healthy
        # CAN stack was reported as broken. vcan creation itself was verified working
        # on hardware while this check was failing (2026-08-02).
        sleep 1                                     # let candump bind before sending
        $SUDO cansend vcanTEST 123#DEADBEEF 2>/dev/null
        i=0; while [ "$i" -lt 5 ] && ! grep -qi DEADBEEF "$VF" 2>/dev/null; do sleep 1; i=$((i+1)); done
        $SUDO kill "$CPID" 2>/dev/null
        grep -qi DEADBEEF "$VF" 2>/dev/null && ok "vcan loopback: frame sent and received (stack + can-utils OK)" || no "vcan loopback: frame not received"
        rm -f "$VF"; $SUDO ip link del vcanTEST 2>/dev/null
    else sk "could not create vcanTEST (CONFIG_CAN_VCAN?)"; fi
else no "cansend/candump missing"; fi

# ---------------------------------------------------------------------------
if [ "$LOOPBACK" = "1" ]; then
hdr "6b. HARDWARE CAN loopback  (§7: classic frame on can0, FD frame on can1)"
info "NOTE: this reconfigures can0/can1 into loopback mode. Reboot to restore."
hw_loop(){ # $1=iface $2=fdargs $3=frame
    ifc=$1; fdargs=$2; frame=$3
    $SUDO ip link set "$ifc" down 2>/dev/null
    # shellcheck disable=SC2086
    if $SUDO ip link set "$ifc" type can bitrate 500000 $fdargs loopback on 2>/dev/null && $SUDO ip link set "$ifc" up 2>/dev/null; then
        # candump's own -T (idle timeout, ms) rather than timeout(1): the minimal
        # rootfs has no timeout binary. Integer sleep for busybox compatibility.
        OUT=$( ($SUDO candump -T 3000 -n 1 "$ifc" & sleep 1; $SUDO cansend "$ifc" "$frame"; wait) 2>/dev/null )
        echo "$OUT" | grep -qi "${frame##*#}" && ok "$ifc hardware loopback OK ($frame)" || no "$ifc hardware loopback: no frame (controller/oscillator issue?)"
    else no "$ifc could not enter loopback mode"; fi
}
hw_loop can0 "" "123#DEADBEEF"
hw_loop can1 "dbitrate 1000000 fd on" "456##1.11223344556677889900AABBCCDDEEFF"
info "run 'sudo reboot' to restore can0/can1 to their networkd config"
fi

# ---------------------------------------------------------------------------
hdr "7. WiFi AP  (§7: hostapd up on assigned non-DFS 5GHz channel)"
# hostapd comes up a little after boot (after the wlan0 device + provisioning);
# wait a bit so a fresh-boot run doesn't race it.
i=0; while [ "$i" -lt 20 ] && ! systemctl is-active --quiet ttos-ap 2>/dev/null; do sleep 1; i=$((i+1)); done
if systemctl is-active --quiet ttos-ap 2>/dev/null; then
    ok "ttos-ap (hostapd) service active"
else
    no "ttos-ap service not active"; info "check: systemctl status ttos-ap; journalctl -u ttos-ap"
fi
if have iw; then
    WI=$($SUDO iw dev wlan0 info 2>/dev/null)
    echo "$WI" | grep -qi 'type AP' && ok "wlan0 in AP mode" || no "wlan0 not in AP mode"
    CH=$(echo "$WI" | grep -o 'channel [0-9]*' | awk '{print $2}')
    TXP=$(echo "$WI" | grep -o 'txpower [0-9.]* dBm' )
    info "channel: ${CH:-?}   ${TXP:-txpower ?}"
    case " 36 40 44 48 149 153 157 161 " in *" $CH "*) ok "channel $CH is non-DFS" ;; *) no "channel ${CH:-?} is NOT in the non-DFS set" ;; esac
else sk "iw not available"; fi
IPW=$(ip -4 addr show wlan0 2>/dev/null | grep -o 'inet [0-9.]*' | awk '{print $2}')
[ -n "$IPW" ] && ok "wlan0 has address $IPW (networkd DHCP server)" || no "wlan0 has no IPv4 address"
sk "laptop association + DHCP lease -- verify from a client device"

# ---------------------------------------------------------------------------
hdr "8. Ethernet  (§7: predictable address alongside the AP)"
ETH=$(for i in eth0 end0; do ip link show "$i" >/dev/null 2>&1 && echo "$i" && break; done)
if [ -n "$ETH" ]; then
    IPE=$(ip -4 addr show "$ETH" 2>/dev/null | grep -o 'inet [0-9.]*' | awk '{print $2}')
    [ -n "$IPE" ] && ok "$ETH has address $IPE" || sk "$ETH up but no address yet (no DHCP/cable?) -- link-local should still exist"
else no "no ethernet interface (eth0/end0) found"; fi

# ---------------------------------------------------------------------------
hdr "9. Security  (§5.5: root locked, root SSH refused, no default creds)"
RSTAT=$($SUDO passwd -S root 2>/dev/null | awk '{print $2}')
case "$RSTAT" in L|LK) ok "root account is locked ($RSTAT)" ;; *) no "root not locked (status=$RSTAT)" ;; esac
if have sshd; then
    PRL=$($SUDO sshd -T 2>/dev/null | awk '/^permitrootlogin/{print $2}')
    [ "$PRL" = "no" ] && ok "sshd PermitRootLogin no" || no "sshd PermitRootLogin=$PRL (expected no)"
else
    grep -qiE '^\s*PermitRootLogin\s+no' /etc/ssh/sshd_config 2>/dev/null && ok "sshd_config: PermitRootLogin no" || sk "cannot verify sshd config"
fi
id ttos >/dev/null 2>&1 && ok "ops user 'ttos' exists" || no "ops user 'ttos' missing"
id ttos 2>/dev/null | grep -q 'wheel' && ok "ttos is in 'wheel' (sudo)" || no "ttos not in wheel"
TSTAT=$($SUDO passwd -S ttos 2>/dev/null | awk '{print $2}')
case "$TSTAT" in P|PS) ok "ttos has a password set (provisioned)" ;; *) no "ttos password not set (status=$TSTAT) -- provisioning?" ;; esac

# ---------------------------------------------------------------------------
hdr "10. Provisioning  (§5.6: idempotent, consume+delete, per-car host keys)"
[ -e /etc/ttos/provisioned ] && ok "completion marker present (idempotent guard)" || no "no /etc/ttos/provisioned marker"
if [ -f /boot/ttos-provision.conf ] || [ -f /boot/firmware/ttos-provision.conf ]; then
    no "provisioning file STILL on the FAT partition (should be consumed+deleted)"
else ok "provisioning file removed from FAT partition"; fi
if [ -f /etc/ttos/provision.src ]; then
    PERM=$(stat -c '%a' /etc/ttos/provision.src 2>/dev/null)
    [ "$PERM" = "600" ] && ok "provisioning data moved to rootfs (provision.src, mode 600)" || no "provision.src mode is $PERM (expected 600)"
else sk "no /etc/ttos/provision.src (older provisioning path?)"; fi
HKN=$(ls /etc/ssh/ssh_host_*_key 2>/dev/null | wc -l)
[ "${HKN:-0}" -ge 1 ] && ok "SSH host keys present ($HKN) -- compare fingerprints across two cars to confirm uniqueness" || no "no SSH host keys"
# A LIVE provisioning file (not the shipped .example) carrying a crypt hash would
# be real leakage. The .example placeholder is expected and ignored.
FATHIT=0
for f in /boot/ttos-provision.conf /boot/firmware/ttos-provision.conf; do
    [ -f "$f" ] && $SUDO grep -qE '\$(6|y|2[aby]|5|7)\$' "$f" 2>/dev/null && FATHIT=1
done
[ "$FATHIT" = 0 ] && ok "no live provisioning file with a hash on the FAT partition (example placeholder ignored)" \
                  || no "a provisioning file with a crypt hash is still on the FAT partition"

# ---------------------------------------------------------------------------
hdr "11. USB serial console  (bench aid, §5.5 g_serial -- remove before the event)"
if [ -e /dev/ttyGS0 ]; then
    ok "/dev/ttyGS0 present (dwc2 + g_serial gadget up)"
    if systemctl is-active --quiet serial-getty@ttyGS0.service 2>/dev/null; then
        ok "login getty active on ttyGS0 (this is the console you logged in on)"
    else no "serial-getty@ttyGS0 not active"; fi
    $SUDO grep -q 'use_acm=1' /etc/modprobe.d/ttos-usb-serial.conf 2>/dev/null && ok "g_serial in CDC-ACM mode (portable driverless enumeration)" || info "check use_acm=1 in /etc/modprobe.d/ttos-usb-serial.conf"
else
    sk "/dev/ttyGS0 not present -- expected only on real hardware with the USB-C cable to a host"
fi

# ---------------------------------------------------------------------------
# CTF PRE-EVENT GATE. Everything above answers "does this car work?". This block
# answers "is this car safe to put in front of contestants?" -- a different and
# stricter question. Run it on every car before the doors open.
hdr "12. CTF pre-event gate  (run before a car goes on the floor)"

# Factory mode is a total bypass: open AP, no PSK, driving enabled, console
# ttos/ttos. A car that reaches the venue unprovisioned is a free win AND an open
# network. This is the single most important check in the script.
if [ -e /etc/ttos/factory ]; then
    no "FACTORY/TEST MODE ACTIVE -- open AP, no password, driving enabled. DO NOT DEPLOY."
    info "provision this car (drop ttos-provision.conf on the FAT partition and reboot)"
else
    ok "not in factory/test mode"
fi

case "$VARIANT" in
    *production*) ok "production image (bench images have an empty root password)" ;;
    *) no "NOT the production image -- do not put this car on the floor" ;;
esac

# CAN-over-TCP servers must not ship. socketcand bridged both buses to TCP 29536
# with no authentication, which bypassed every challenge; bcmserver, canlogserver
# and cannelloni are the same class of thing. They ride along in can-utils-access
# (which we need for cangw), so the image recipe deletes them at rootfs assembly --
# this check is what catches that deletion silently breaking.
CANSRV=""
for b in socketcand bcmserver canlogserver cannelloni; do
    have "$b" && CANSRV="$CANSRV $b"
done
systemctl list-unit-files 2>/dev/null | grep -q '^socketcand' && CANSRV="$CANSRV socketcand.service"
if [ -n "$CANSRV" ]; then
    no "unauthenticated CAN-over-TCP server(s) present:$CANSRV -- must not ship on a competition car"
else
    ok "no CAN-over-TCP servers (diagnostic access is the physical tap only)"
fi

# The kernel gateway is the whole basis of the DRIVE/DIAG separation.
if have cangw; then
    ok "cangw available"
    RULES=$($SUDO cangw -L 2>/dev/null | grep -c 'cangw -A' || true)
    info "active gateway rules: ${RULES:-0}"
else
    no "cangw missing -- gateway policy cannot be applied (CONFIG_CAN_GW / can-utils)"
fi

# Raw frame views must be gated, or a locked panel leaks the C2 corpus.
DASHFRAMES=$(sed -n 's/^TTOS_DASH_FRAMES=//p' /etc/default/ttos-dashboard 2>/dev/null | head -n 1)
if [ -z "$DASHFRAMES" ]; then
    ok "dashboard raw frame streaming disabled (TTOS_DASH_FRAMES empty)"
else
    no "TTOS_DASH_FRAMES=$DASHFRAMES -- the panel will stream raw frames to anyone"
fi

# ---------------------------------------------------------------------------
hdr "Summary"
printf "  ${G}PASS %d${N}   ${R}FAIL %d${N}   ${Y}SKIP %d${N}\n" "$PASS" "$FAIL" "$SKIP"
printf "  SKIP items need a second machine or manual verification (see notes above).\n"
[ "$FAIL" -eq 0 ] && { printf "\n${G}${B}All automated checks passed.${N}\n"; exit 0; } || { printf "\n${R}${B}%d check(s) failed -- see above.${N}\n" "$FAIL"; exit 1; }
