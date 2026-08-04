PACKAGECONFIG:append = " networkd resolved"
FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

# CTF network configuration. Dropped vs the old layer:
#   - usb0.network  (§4.7 USB Ethernet gadget removed)
#   - vcan0.netdev / vcan0.network (§5.2 -- two real CAN interfaces, no vcan)
SRC_URI += "\
    file://10-ttos-can.rules \
    file://ttos-journald.conf \
    file://candrive.network \
    file://candiag.network \
    file://20-wired.network \
    file://30-wlan-ap.network \
"

FILES:${PN} += "${sysconfdir}/udev/rules.d/ ${sysconfdir}/systemd/network/ ${sysconfdir}/systemd/journald.conf.d/"

do_install:append() {
    install -d ${D}${sysconfdir}/systemd/network/
    install -d ${D}${sysconfdir}/udev/rules.d
    install -m 0644 ${WORKDIR}/10-ttos-can.rules  ${D}${sysconfdir}/udev/rules.d/
    install -d ${D}${sysconfdir}/systemd/journald.conf.d
    install -m 0644 ${WORKDIR}/ttos-journald.conf ${D}${sysconfdir}/systemd/journald.conf.d/
    install -m 0644 ${WORKDIR}/candrive.network   ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/candiag.network    ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/20-wired.network   ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/30-wlan-ap.network ${D}${sysconfdir}/systemd/network/
}
