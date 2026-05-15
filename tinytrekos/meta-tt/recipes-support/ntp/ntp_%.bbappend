FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

SRC_URI += "\
    file://ntp.conf \
"

FILES:${PN} += "${sysconfdir}/ntp.conf"

do_install:append() {
    install -m 0644 ${WORKDIR}/ntp.conf ${D}${sysconfdir}/ntp.conf
}