#!/bin/sh
# Pre-flight for hostapd: make sure the radio is actually ready BEFORE hostapd
# runs. Without this, hostapd can start before the brcmfmac driver has created
# wlan0, or before the regulatory domain has been applied -- it exits immediately
# and (with a restart limit) can give up for the rest of the boot. That is the
# "no AP on some boots" failure. Best-effort: never fails the unit.
set -u
[ -f /etc/default/ttos-wifi ] && . /etc/default/ttos-wifi
COUNTRY="${TTOS_WIFI_COUNTRY:-US}"

log() { logger -t ttos-ap "$*" 2>/dev/null; printf 'ttos-ap: %s\n' "$*"; }

# 1. Wait for the wlan0 interface to exist (driver/firmware load is asynchronous
#    and is slower on a cold boot -- hence the intermittency).
i=0
while [ "$i" -lt 30 ]; do
    [ -e /sys/class/net/wlan0 ] && break
    i=$((i + 1))
    sleep 1
done
if [ ! -e /sys/class/net/wlan0 ]; then
    log "WARNING: wlan0 did not appear after ${i}s; letting hostapd try anyway"
else
    [ "$i" -gt 0 ] && log "wlan0 appeared after ${i}s"
fi

# 2. Clear any rfkill soft-block (a blocked radio makes hostapd fail to start).
if command -v rfkill >/dev/null 2>&1; then
    rfkill unblock wifi 2>/dev/null || rfkill unblock all 2>/dev/null || true
fi

# 3. Apply the regulatory domain and let it settle. This matters most on 5 GHz:
#    under the default world domain ("00") the 5 GHz channels are no-IR (passive
#    scan only), so hostapd cannot start a BSS there and exits.
if command -v iw >/dev/null 2>&1; then
    iw reg set "$COUNTRY" 2>/dev/null || true
    j=0
    while [ "$j" -lt 10 ]; do
        cur=$(iw reg get 2>/dev/null | awk '/^country/{print $2; exit}' | tr -d ':')
        [ "$cur" = "$COUNTRY" ] && break
        j=$((j + 1))
        sleep 1
    done
    [ "$j" -gt 0 ] && log "regulatory domain settled to ${cur:-unknown} after ${j}s"
fi

# 4. Make sure the interface is down before hostapd claims it (a half-configured
#    interface left by a previous failed attempt also makes hostapd exit).
ip link set wlan0 down 2>/dev/null || true

exit 0
