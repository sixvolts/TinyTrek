SUMMARY = "TTOS CTF on-target tools (self-test)"
DESCRIPTION = "Installs ttos-selftest to /usr/bin so it ships on every car: log in on \
the console and run 'ttos-selftest' to verify CAN, cangw, WiFi AP, networking, \
security, and provisioning against the acceptance criteria (§7)."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = "file://ttos-selftest.sh file://ttos-reset.sh file://ttos-provision-nodes.sh"

inherit allarch

# Runtime tools the self-test invokes.
RDEPENDS:${PN} = "can-utils iproute2 iw"

do_install() {
    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/ttos-selftest.sh ${D}${bindir}/ttos-selftest
    install -m 0755 ${WORKDIR}/ttos-reset.sh    ${D}${bindir}/ttos-reset
    install -m 0755 ${WORKDIR}/ttos-provision-nodes.sh ${D}${bindir}/ttos-provision-nodes
}

FILES:${PN} = "${bindir}/ttos-selftest ${bindir}/ttos-reset ${bindir}/ttos-provision-nodes"
