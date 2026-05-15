SUMMARY = "Setup OverlayFS for SSH keys to persist across updates"
DESCRIPTION = "Automates the setup of OverlayFS to make SSH keys persistent across Mender updates by using directories on the /data partition."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

inherit systemd

# Install custom service and setup script
SRC_URI = " \
    file://ssh-overlay.service \
    file://ssh-overlay-setup.sh \
"

FILES:${PN} += "\
    /usr/bin/ssh-overlay-setup.sh \
    ${systemd_system_unitdir}/ssh-overlay.service \
"

SYSTEMD_SERVICE:${PN} = "ssh-overlay.service"

do_install() {
    # Install service file
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/ssh-overlay.service ${D}${systemd_system_unitdir}/

    # Install setup script
    install -d ${D}/usr/bin
    install -m 0755 ${WORKDIR}/ssh-overlay-setup.sh ${D}/usr/bin/ssh-overlay-setup.sh
}

# Enable the service by default
SYSTEMD_AUTO_ENABLE = "enable"
