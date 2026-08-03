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

# iproute2's `ip` and `iw` live in /sbin, which is NOT in the ttos user's PATH.
# Without this every interface check reports "does not exist" -- on a car whose
# interfaces are demonstrably up. Set before anything else runs.
PATH="/sbin:/usr/sbin:$PATH"
export PATH

# Nearly every check here needs root. Run without it and the output is a page of
# false failures (can0/can1 "missing", wlan0 with no address, no ethernet), which
# is far worse than refusing to run: it teaches whoever is holding the car to
# ignore the tool. Re-exec once under sudo instead -- one password prompt at most,
# rather than one per privileged command.
if [ "$(id -u)" != 0 ]; then
    if command -v sudo >/dev/null 2>&1; then
        exec sudo -- "$0" "$@"
    fi
    printf '\n*** NOT RUNNING AS ROOT and sudo is unavailable. ***\n'
    printf '    Interface, gateway and account checks will report FALSE FAILURES.\n'
    printf '    Re-run as: sudo %s\n\n' "$0"
fi

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
# Role names, not numbers -- DRIVE is the HIGHER-numbered interface here (can1),
# the opposite of the original design doc.
#   DIAG  can0  CAN FD, 500k/1M  -- 64-byte diagnostic responses, no ISO-TP needed.
#                                   Only contestant test adapters live here.
#   DRIVE can1  CLASSIC 2.0, 500k -- every node on it is classic-only (the motor
#                                   nodes are MCP2515, no FD support whatsoever).
# can1 must NOT report FD: an FD frame is a form error to those nodes and takes the
# drivetrain to bus-off. Keeping FD off means the controller cannot emit one.
hdr "4. CAN interfaces  (DIAG=can0 FD 500k/1M; DRIVE=can1 classic 500k)"
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
    # 'dbitrate' appears only on an FD-configured interface. Use that rather than
    # grepping for "fd": the driver name mcp251xfd contains those letters, so a
    # loose match reports a classic interface as FD.
    DBR=$(echo "$D" | grep -o 'dbitrate [0-9]*' | awk '{print $2}')
    if [ "$fd" = "1" ]; then
        [ -n "$DBR" ] && ok "$ifc is CAN FD" || no "$ifc is not in FD mode"
        [ "$DBR" = "1000000" ] && ok "$ifc data bitrate = 1000000" || no "$ifc dbitrate = ${DBR:-?} (expected 1000000)"
    else
        # Asserting the ABSENCE of FD matters more than asserting its presence:
        # the motor nodes are classic-only MCP2515, so an FD frame on this bus is a
        # form error that drives them error-passive and then bus-off, stopping
        # propulsion. Configuring the bus classic means the controller cannot emit
        # one at all.
        if [ -n "$DBR" ]; then
            no "$ifc is in FD mode (dbitrate $DBR) -- the DRIVE bus must be CLASSIC; FD frames bus-off the MCP2515 nodes"
        else
            ok "$ifc is classic CAN 2.0 (correct for the drive bus)"
        fi
    fi
    [ "$STATE" = "ERROR-ACTIVE" ] && ok "$ifc state ERROR-ACTIVE" || sk "$ifc state = ${STATE:-?} (ERROR-ACTIVE needs a wired, terminated bus)"
}
check_can can0 1   # DIAG: must be FD
check_can can1 0   # DRIVE: must be classic -- FD here would bus-off the motor nodes

# ---------------------------------------------------------------------------
hdr "5. cangw gateway  (go/no-go: cangw works; shipped DRIVE->DIAG policy is live)"
if have cangw; then
    # Judge cangw -L by whether it PRODUCES A LISTING, not by exit status: it exits
    # non-zero even on success (measured: 36 while listing both rules), so the old
    # check reported a working gateway as broken on every car.
    GWL=$($SUDO cangw -L 2>/dev/null)
    if [ -n "$GWL" ]; then
        ok "cangw -L runs"
        # NON-DESTRUCTIVE. The previous version added a test rule and then ran
        # `cangw -F`, which flushes EVERY rule -- so running the self-test on a live
        # car silently disabled the DRIVE/DIAG gateway until the next reboot. That
        # only stayed hidden because the broken exit-status test above bailed out
        # before reaching it. Verify the SHIPPED policy instead of mutating it.
        for gwid in 7D1 7D2; do
            if echo "$GWL" | grep -q -- "-s can1 -d can0 -f ${gwid}:"; then
                ok "gateway rule present: DRIVE(can1) -> DIAG(can0) ${gwid}"
            else
                no "gateway rule MISSING: can1 -> can0 ${gwid} (is ttos-cangw.service running?)"
            fi
        done
        # Nothing may be forwarded toward the vehicle bus in the default policy --
        # an inbound rule here would hand contestants the drive bus.
        if echo "$GWL" | grep -q -- '-s can0 -d can1'; then
            no "INBOUND rule can0 -> can1 present -- nothing should reach the drive bus"
        else
            ok "no inbound DIAG -> DRIVE forwarding (correct default policy)"
        fi
    else
        no "cangw -L listed nothing (policy not applied, or CONFIG_CAN_GW missing)"
        info "restore with: sudo ttos-cangw-policy default"
    fi
else no "cangw binary missing (can-utils not installed?)"; fi

# ---------------------------------------------------------------------------
hdr "6. CAN stack + tools smoke test (vcan loopback -- non-destructive)"
if have cansend && have candump; then
    $SUDO modprobe vcan 2>/dev/null
    if $SUDO ip link add dev vcanTEST type vcan 2>/dev/null && $SUDO ip link set vcanTEST up 2>/dev/null; then
        VF=$(mktemp 2>/dev/null || echo /tmp/vcan.$$)
        # -L (log format) prints "123#DEADBEEF". WITHOUT it candump prints the data
        # bytes SPACE-SEPARATED -- "123   [4]  DE AD BE EF" -- so the grep for
        # DEADBEEF below never matched and a healthy CAN stack was reported broken
        # on every car. Verified on hardware 2026-08-03; the vcan link, cansend and
        # candump were all fine the whole time. This was a test bug, not a fault.
        $SUDO candump -L -n 1 vcanTEST > "$VF" 2>/dev/null &
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
        # -L for the same reason as section 6: default candump space-separates the
        # data bytes, so a grep for the sent payload never matches. Both sides are
        # then reduced to bare hex so FD's "##<flags>" and any separators in the
        # cansend argument cannot break the comparison either.
        OUT=$( ($SUDO candump -L -T 3000 -n 1 "$ifc" & sleep 1; $SUDO cansend "$ifc" "$frame"; wait) 2>/dev/null )
        GOT=$(printf '%s' "$OUT"            | tr -dc '0-9A-Fa-f' | tr 'a-f' 'A-F')
        WANT=$(printf '%s' "${frame##*#}"   | tr -dc '0-9A-Fa-f' | tr 'a-f' 'A-F')
        case "$GOT" in
            *"$WANT"*) ok "$ifc hardware loopback OK ($frame)" ;;
            *) no "$ifc hardware loopback: no frame (controller/oscillator issue?)" ;;
        esac
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
    OWNR=$(stat -c '%U:%G' /etc/ttos/provision.src 2>/dev/null)
    # 640 root:ttos-secrets, NOT 600. ttos-dashboard runs DynamicUser=yes, so a
    # 600 root file is unreadable to it and the car serves no DIDs and no unlock
    # codes while looking completely healthy. The group is the whole access path;
    # check it too, because 640 root:root is just as broken as 600 and looks right.
    if [ "$PERM" = "640" ] && [ "$OWNR" = "root:ttos-secrets" ]; then
        ok "provisioning data staged to rootfs (provision.src, 640 root:ttos-secrets)"
    else
        no "provision.src is $PERM $OWNR (expected 640 root:ttos-secrets -- the dashboard's DynamicUser cannot read it otherwise)"
    fi
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
#
# ASK THE PANEL, do not read the config. Until Phase 5 the only gate was
# TTOS_DASH_FRAMES being empty, so checking the variable was equivalent to checking
# the behaviour. It is not equivalent any more: per-session tier gating is now the
# real control and the buses are deliberately listed, so the old check fails a
# correctly configured car. Worse, the reverse is also possible -- an empty variable
# with broken tier logic would pass. Query what an UNAUTHENTICATED client actually
# receives; that is the property that matters.
DASHADDR=$(sed -n 's/^TTOS_DASH_ADDR=//p' /etc/default/ttos-dashboard 2>/dev/null | head -n 1)
if have curl && [ -n "$DASHADDR" ]; then
    INFO=$(curl -s --max-time 5 "http://${DASHADDR}/api/info" 2>/dev/null)
    if [ -z "$INFO" ]; then
        sk "panel not answering on $DASHADDR (AP down?) -- frame gating unverified"
    elif echo "$INFO" | grep -q '"frames":\[\]'; then
        ok "locked panel exposes no raw frame tabs (tier gating live)"
    else
        no "locked panel advertises frame tabs: $(echo "$INFO" | sed 's/.*"frames"://; s/,".*//') -- a locked car would leak the C2 corpus"
    fi
else
    # Fall back to the blunt check when curl or the address is unavailable.
    DASHFRAMES=$(sed -n 's/^TTOS_DASH_FRAMES=//p' /etc/default/ttos-dashboard 2>/dev/null | head -n 1)
    if [ -z "$DASHFRAMES" ]; then
        ok "dashboard raw frame streaming disabled (TTOS_DASH_FRAMES empty)"
    else
        sk "cannot query the panel; TTOS_DASH_FRAMES=$DASHFRAMES relies on tier gating (verify with bench/test-panel.py)"
    fi
fi

# ---------------------------------------------------------------------------
hdr "Summary"
printf "  ${G}PASS %d${N}   ${R}FAIL %d${N}   ${Y}SKIP %d${N}\n" "$PASS" "$FAIL" "$SKIP"
printf "  SKIP items need a second machine or manual verification (see notes above).\n"
[ "$FAIL" -eq 0 ] && { printf "\n${G}${B}All automated checks passed.${N}\n"; exit 0; } || { printf "\n${R}${B}%d check(s) failed -- see above.${N}\n" "$FAIL"; exit 1; }
