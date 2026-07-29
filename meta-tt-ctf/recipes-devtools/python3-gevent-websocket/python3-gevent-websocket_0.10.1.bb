SUMMARY = "python3-gevent-websocket Recipe"
DESCRIPTION = "Install the gevent-websocket package from PyPI"
HOMEPAGE = "https://pypi.org/project/gevent-websocket/"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI[sha256sum] = "7eaef32968290c9121f7c35b973e2cc302ffb076d018c9068d2f5ca8b2d85fb0"

PYPI_PACKAGE = "gevent-websocket"

DEPENDS += "python3"
RDEPENDS:${PN} += "python3 python3-pip"

inherit pypi setuptools3
