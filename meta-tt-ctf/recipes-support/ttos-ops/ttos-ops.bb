SUMMARY = "TTOS CTF ops privileges (sudoers for wheel group)"
DESCRIPTION = "Grants the 'wheel' group full sudo. The 'ttos' ops user belongs to wheel; \
root is locked, so this is how the serial ops console gains privilege (§5.5)."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://wheel-sudo"

inherit allarch

RDEPENDS:${PN} = "sudo"

do_install() {
    install -d ${D}${sysconfdir}/sudoers.d
    install -m 0440 ${WORKDIR}/wheel-sudo ${D}${sysconfdir}/sudoers.d/wheel
}

FILES:${PN} = "${sysconfdir}/sudoers.d/wheel"
