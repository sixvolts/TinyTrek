SUMMARY = "TTOS CTF WiFi Access Point (hostapd + networkd DHCP)"
DESCRIPTION = "Ships the hostapd AP config template and service. SSID, PSK, channel, \
country and TX power are per-car provisioning data (§5.3/§5.6), rendered on first boot. \
DHCP is served by systemd-networkd (see 30-wlan-ap.network), not dnsmasq."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://hostapd.conf.template \
    file://ttos-ap.service \
    file://ttos-ap-txpower.sh \
    file://ttos-wifi.default.example \
"

inherit systemd allarch

# hostapd (meta-networking) + iw for the txpower knob.
RDEPENDS:${PN} = "hostapd iw"

SYSTEMD_SERVICE:${PN} = "ttos-ap.service"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    install -d ${D}${sysconfdir}/hostapd
    install -m 0644 ${WORKDIR}/hostapd.conf.template ${D}${sysconfdir}/hostapd/hostapd.conf.template

    install -d ${D}${sysconfdir}/default
    install -m 0644 ${WORKDIR}/ttos-wifi.default.example ${D}${sysconfdir}/default/ttos-wifi.example

    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/ttos-ap-txpower.sh ${D}${bindir}/ttos-ap-txpower

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/ttos-ap.service ${D}${systemd_system_unitdir}/ttos-ap.service
}

FILES:${PN} += " \
    ${sysconfdir}/hostapd/hostapd.conf.template \
    ${sysconfdir}/default/ttos-wifi.example \
    ${bindir}/ttos-ap-txpower \
    ${systemd_system_unitdir}/ttos-ap.service \
"
