SUMMARY = "TTOS CTF CAN gateway policy"
DESCRIPTION = "Applies the in-kernel cangw policy separating the DRIVE bus (can1: \
motors, BMS, heartbeat) from the DIAG bus (can0: the contestant side tap). Default \
policy forwards the flag frames 0x7D1/0x7D2 outbound only; nothing is forwarded \
inbound until a challenge opens the bridge window. Classic frames only -- the DRIVE \
bus nodes are classic-only MCP2515 and a real FD frame can drive them to bus-off."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://ttos-cangw-policy.sh \
    file://ttos-cangw.service \
"

inherit systemd allarch

# cangw(1) ships in can-utils. CONFIG_CAN_GW=y is supplied by the kernel fragment
# in recipes-kernel/linux (attached to linux-raspberrypi, which is the kernel that
# is actually built here -- a linux-yocto bbappend would silently never apply).
RDEPENDS:${PN} = "can-utils"

SYSTEMD_SERVICE:${PN} = "ttos-cangw.service"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/ttos-cangw-policy.sh ${D}${bindir}/ttos-cangw-policy

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/ttos-cangw.service ${D}${systemd_system_unitdir}/ttos-cangw.service
}

FILES:${PN} += " \
    ${bindir}/ttos-cangw-policy \
    ${systemd_system_unitdir}/ttos-cangw.service \
"
