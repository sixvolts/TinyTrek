FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

SRC_URI += "\
    file://defaults.vim \
"

FILES:${PN} += " \
    ${datadir}/vim/${VIMDIR}/defaults.vim \
"

do_install:append() {
    install -d ${D}${datadir}/vim/${VIMDIR}
    install -m 0644 ${WORKDIR}/defaults.vim ${D}${datadir}/vim/${VIMDIR}/defaults.vim
}