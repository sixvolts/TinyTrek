PACKAGECONFIG:append = " networkd resolved"
FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

SRC_URI += "\
    file://usb0.network \
    file://can1.network \
    file://vcan0.network \
    file://vcan0.netdev \
"

FILES:${PN} += "${sysconfdir}/systemd/network/"
FILES:${PN} += "${sysconfdir}/systemd/network/usb0.network"
FILES:${PN} += "${sysconfdir}/systemd/network/can1.network"
FILES:${PN} += "${sysconfdir}/systemd/network/vcan0.network"
FILES:${PN} += "${sysconfdir}/systemd/network/vcan0.netdev"

do_install:append() {
    install -d ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/usb0.network ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/can1.network ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/vcan0.network ${D}${sysconfdir}/systemd/network/
    install -m 0644 ${WORKDIR}/vcan0.netdev ${D}${sysconfdir}/systemd/network/
}