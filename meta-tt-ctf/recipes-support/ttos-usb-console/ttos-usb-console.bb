SUMMARY = "USB CDC-ACM serial ops console (g_serial on the USB-C port)"
DESCRIPTION = "Loads the dwc2 peripheral controller + g_serial (CDC-ACM) and runs a \
login getty on /dev/ttyGS0, so the ops console is reachable over the USB-C port with \
no HDMI/monitor. TEMPORARY bench aid (§5.5 g_serial path); expected to be removed \
before the event. Login is the password-gated 'ttos' account, same as the GPIO UART."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://ttos-usb-serial.load \
    file://ttos-usb-serial.options \
"

inherit allarch

# serial-getty@.service is provided by systemd.
RDEPENDS:${PN} = "systemd"

do_install() {
    install -d ${D}${sysconfdir}/modules-load.d
    install -m 0644 ${WORKDIR}/ttos-usb-serial.load ${D}${sysconfdir}/modules-load.d/ttos-usb-serial.conf

    install -d ${D}${sysconfdir}/modprobe.d
    install -m 0644 ${WORKDIR}/ttos-usb-serial.options ${D}${sysconfdir}/modprobe.d/ttos-usb-serial.conf

    # Enable a login getty on the gadget tty. serial-getty@.service already carries
    # BindsTo/After dev-ttyGS0.device, so it starts once g_serial creates the node.
    install -d ${D}${sysconfdir}/systemd/system/getty.target.wants
    ln -sf ${systemd_system_unitdir}/serial-getty@.service \
        ${D}${sysconfdir}/systemd/system/getty.target.wants/serial-getty@ttyGS0.service
}

FILES:${PN} = " \
    ${sysconfdir}/modules-load.d/ttos-usb-serial.conf \
    ${sysconfdir}/modprobe.d/ttos-usb-serial.conf \
    ${sysconfdir}/systemd/system/getty.target.wants/serial-getty@ttyGS0.service \
"
