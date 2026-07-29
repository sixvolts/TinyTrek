PACKAGECONFIG:append = " networkd resolved"
FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

# CTF network configuration. Dropped vs the old layer:
#   - usb0.network  (§4.7 USB Ethernet gadget removed)
#   - vcan0.netdev / vcan0.network (§5.2 -- two real CAN interfaces, no vcan)
SRC_URI += "\
    file://10-can0.link \
    file://10-can1.link \
    file://can0.network \
    file://can1.network \
    file://20-wired.network \
    file://30-wlan-ap.network \
"

FILES:${PN} += "${sysconfdir}/systemd/network/"

do_install:append() {
    install -d ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/10-can0.link       ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/10-can1.link       ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/can0.network       ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/can1.network       ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/20-wired.network   ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/30-wlan-ap.network ${D}${sysconfdir}/systemd/network/
}
