# §4.1 FIX (HIGH PRIORITY)
# meta-raspberrypi sets PREFERRED_PROVIDER_virtual/kernel = "linux-raspberrypi",
# so the old layer's linux-yocto_6.6.bbappend NEVER applied and its can_config.cfg
# was dead code -- CONFIG_CAN_GW was never guaranteed in the shipped kernel.
# The whole CTF gateway design depends on in-kernel cangw, so the fragment now
# lives HERE, on the kernel that is actually built.
#
# Verify on a booted image (do not assume):
#   zcat /proc/config.gz | grep -E 'CAN_GW|MCP251XFD'
#   cangw -L
FILESEXTRAPATHS:prepend := "${THISDIR}/${PN}:"

SRC_URI += "file://can_config.cfg"

# Ship the device-tree overlays we reference from config.txt into the boot
# partition's overlays/ dir. mcp251xfd is the driver for the MCP2518FD on the
# Waveshare 2-CH CAN FD HAT (NOT mcp2515 -- that was the old MCP2515 hardware).
# spi1-3cs enables the second SPI bus needed for HAT "Mode A" (dual independent SPI).
RPI_KERNEL_DEVICETREE_OVERLAYS:append = " overlays/mcp251xfd.dtbo overlays/spi1-3cs.dtbo"
