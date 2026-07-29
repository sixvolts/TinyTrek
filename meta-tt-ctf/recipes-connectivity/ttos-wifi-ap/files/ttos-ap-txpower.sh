#!/bin/sh
# Set a low fixed WiFi TX power so association is only practical within ~1-2 m of
# the car (§5.3). Value comes from provisioning via /etc/default/ttos-wifi.
[ -f /etc/default/ttos-wifi ] && . /etc/default/ttos-wifi
MBM="${TTOS_WIFI_TXPOWER_MBM:-500}"   # 500 mBm = 5 dBm

# hostapd may still be bringing wlan0 into AP mode; give it a moment.
i=0
while [ "$i" -lt 5 ]; do
    iw dev wlan0 info >/dev/null 2>&1 && break
    i=$((i + 1))
    sleep 1
done

if iw dev wlan0 set txpower fixed "$MBM"; then
    logger -t ttos-ap "wlan0 txpower set to ${MBM} mBm"
else
    logger -t ttos-ap "WARNING: failed to set wlan0 txpower to ${MBM} mBm"
fi
