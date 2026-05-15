DESCRIPTION = "TTOS Base Image"
LICENSE = "MIT"

inherit core-image

SDKMACHINE = "x86_64"

IMAGE_FEATURES:append = " ssh-server-openssh"
IMAGE_FEATURES:append = " tools-sdk dev-pkgs tools-debug tools-profile tools-testapps"
IMAGE_FEATURES:remove = " wayland x11"

EXTRA_IMAGE_FEATURES = "debug-tweaks tools-sdk tools-debug"

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
