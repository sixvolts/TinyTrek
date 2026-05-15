LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://LICENSE;md5=4c2285159e2829755e52ad79ec6fb71f"

SRC_URI = " \
    git://git@git.faro.re/capt_redbeard/tinytrek_web.git;protocol=ssh;branch=main \
    file://tinytrek.service \
"

SRCREV = "${AUTOREV}"
BPV = "0.1.0"
PV = "${BPV}+git${SRCPV}"

S = "${WORKDIR}/git"

FILES:${PN} += "/opt/tinytrek \
                ${systemd_unitdir}/system/tinytrek.service \
"

inherit systemd
inherit python_poetry_core

RDEPENDS:${PN} = "python3 python3-flask python3-flask-socketio python3-can python3-cantools python3-gevent python3-gevent-websocket python3-simple-websocket"

do_install:append() {
    install -d ${D}${systemd_unitdir}/system
    install -m 0644 ${WORKDIR}/tinytrek.service ${D}${systemd_unitdir}/system

    install -d ${D}/opt/tinytrek
    cp -r ${S}/* ${D}/opt/tinytrek
}

SYSTEMD_SERVICE:${PN} = "tinytrek.service"
