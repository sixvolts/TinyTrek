DESCRIPTION = "TinyTrekOS CTF baseline -- PRODUCTION image for competition cars"
LICENSE = "MIT"

inherit core-image extrausers

# ssh for development. NO debug-tweaks here (§4.8) -- that is bench-only and yields
# an empty root password. root is locked and the 'ttos' password is set only by
# first-boot provisioning (§5.5/§5.6).
IMAGE_FEATURES += "ssh-server-openssh"

IMAGE_INSTALL:append = " \
    kernel-image kernel-devicetree \
    ntp sntp \
    curl openssl vim-tiny \
    can-utils can-utils-access iproute2 \
    bash \
    hostapd iw ttos-wifi-ap \
    sudo ttos-ops \
    ttos-cangw \
    ttos-growfs \
    ttos-provision \
    ttos-usb-console \
    ttos-tools \
    ttos-dashboard \
    python3 python3-core \
"

# socketcand REMOVED (decision 2026-08-02). It was a TCP bridge to both CAN buses on
# port 29536 -- an unauthenticated network path to the DRIVE bus that bypassed every
# challenge. Restricting it to the DIAG bus would not have saved it either: socketcand
# is only a front-end to the Pi's own can0 controller, so it adds no second node to
# ACK, and a contestant with no physical tap cannot transmit or receive regardless.
# Diagnostic access is now the physical side tap only. The recipe is left in the layer
# (unbuilt) rather than deleted; do not re-add it to any image that ships on a car.
# The C3 relay is a separate, authenticated service -- it does NOT reuse socketcand.

# Accounts (§5.5):
#  - root: locked -> no login on any path.
#  - ttos: ops/dev user in 'wheel' (sudo via ttos-ops), locked until provisioning
#          sets its password hash on first boot.
EXTRA_USERS_PARAMS = "\
    usermod -L root; \
    groupadd -f wheel; \
    useradd -m -G wheel -s /bin/bash ttos; \
    usermod -L ttos; \
"

# CAN overlays onto the FAT boot partition (referenced from config.txt, see rpi-config).
# These two are NOT in meta-raspberrypi's default KERNEL_DEVICETREE overlay set, so
# they must be staged explicitly. Syntax is "<deploy-file>;<dest-on-boot>" — the
# kernel deploys the overlays flat (mcp251xfd.dtbo), and config.txt loads them from
# overlays/ on the FAT partition. (disable-bt.dtbo is already staged by the default.)
IMAGE_BOOT_FILES:append = " mcp251xfd.dtbo;overlays/mcp251xfd.dtbo spi1-3cs.dtbo;overlays/spi1-3cs.dtbo"

# Ship the provisioning template on the FAT boot partition so a flashed card is
# ready to edit in place (§5.6 workflow). ttos-provision deploys it to DEPLOY_DIR_IMAGE.
IMAGE_BOOT_FILES:append = " ttos-provision.conf.example;ttos-provision.conf.example"
do_image_wic[depends] += "ttos-provision:do_deploy"

# Variant marker so prod vs bench is unmistakable (§4.8). Hostname stays
# "ttos-ctf-unprovisioned" until provisioning sets the real per-car name.
ROOTFS_POSTPROCESS_COMMAND += "ttos_variant_marker; "
ttos_variant_marker() {
    echo "TinyTrekOS-CTF (production)" > ${IMAGE_ROOTFS}${sysconfdir}/ttos-variant
}
