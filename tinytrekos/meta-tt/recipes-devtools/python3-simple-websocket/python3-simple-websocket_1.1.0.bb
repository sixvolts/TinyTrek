SUMMARY = "python3-simple-websocket Recipe"
DESCRIPTION = "Install the simple_websocket package from PyPI"
HOMEPAGE = "https://pypi.org/project/simple-websocket/"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI[sha256sum] = "7939234e7aa067c534abdab3a9ed933ec9ce4691b0713c78acb195560aa52ae4"

PYPI_PACKAGE = "simple_websocket"

DEPENDS += "python3"
RDEPENDS_${PN} += "python3 python3-pip"

inherit pypi python_poetry_core
