DESCRIPTION = "TTOS Base Image"
LICENSE = "MIT"

inherit core-image

IMAGE_FEATURES:append = " ssh-server-openssh"
IMAGE_FEATURES:remove = " wayland x11"

IMAGE_INSTALL:append = " \
    dhcpcd \
    ntp \
    sntp \
    curl \
    openssl \
    vim-tiny \
    can-utils \
    iproute2 \
    tinytrek \
"

IMAGE_BOOT_FILES:append = " spi1-3cs.dtbo;overlays/spi1-3cs.dtbo mcp2515.dtbo;overlays/mcp2515.dtbo"
