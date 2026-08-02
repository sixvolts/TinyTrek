SUMMARY = "TTOS CTF first-boot root filesystem expansion"
DESCRIPTION = "Grows the root partition to fill the SD card and online-resizes the \
ext4 on first boot, once (marker in /etc/ttos/rootfs-expanded). Only ever extends \
the last partition into trailing free space; runs before provisioning."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://ttos-growfs.sh \
    file://ttos-growfs.service \
"

inherit systemd allarch

# Tools the script needs that the minimal rootfs does not already carry:
#  - resize2fs        grow the ext4 (online, on the mounted root)
#  - sfdisk           extend the partition-table entry to fill the disk
#  - partx            make the kernel adopt the new (mounted) partition size
RDEPENDS:${PN} = "e2fsprogs-resize2fs util-linux-sfdisk util-linux-partx"

SYSTEMD_SERVICE:${PN} = "ttos-growfs.service"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    install -d ${D}${sbindir}
    install -m 0755 ${WORKDIR}/ttos-growfs.sh ${D}${sbindir}/ttos-growfs

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/ttos-growfs.service ${D}${systemd_system_unitdir}/ttos-growfs.service
}

FILES:${PN} += " \
    ${sbindir}/ttos-growfs \
    ${systemd_system_unitdir}/ttos-growfs.service \
"
