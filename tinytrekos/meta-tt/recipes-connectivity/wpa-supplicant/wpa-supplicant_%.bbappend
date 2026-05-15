FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

SRC_URI += "\
    file://wpa_supplicant-wlan0.conf \
"

FILES:${PN} += "${sysconfdir}/wpa_supplicant"
FILES:${PN} += "${sysconfdir}/wpa_supplicant/wpa_supplicant-wlan0.conf"

do_install:append() {
    install -d ${D}${sysconfdir}/wpa_supplicant
    install -m 0644 ${WORKDIR}/wpa_supplicant-wlan0.conf ${D}${sysconfdir}/wpa_supplicant/wpa_supplicant-wlan0.conf
    sed -i -e 's/SSID/${TTOS_WIFI_SSID}/' ${D}${sysconfdir}/wpa_supplicant/wpa_supplicant-wlan0.conf
    sed -i -e 's/PSK/${TTOS_WIFI_PSK}/' ${D}${sysconfdir}/wpa_supplicant/wpa_supplicant-wlan0.conf

    install -d ${D}${sysconfdir}/systemd/system/multi-user.target.wants
    ln -s ${D}/usr/lib/systemd/system/wpa_supplicant@.service ${D}${sysconfdir}/systemd/system/multi-user.target.wants/wpa_supplicant@wlan0.service
}