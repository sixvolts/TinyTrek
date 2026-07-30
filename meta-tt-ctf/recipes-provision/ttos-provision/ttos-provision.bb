SUMMARY = "TTOS CTF first-boot provisioning service (§5.6)"
DESCRIPTION = "Oneshot systemd service that applies per-car values (hostname, car id, \
WiFi SSID/PSK/channel, serial console password hash, per-car SSH host keys) from a \
plain-text file on the FAT boot partition. Idempotent, crash-safe, consume-and-delete, \
fails loudly if the file is missing or malformed."
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://ttos-provision.sh \
    file://ttos-provision.service \
    file://ttos-provision.conf.example \
"

inherit systemd deploy

# chpasswd/usermod -> shadow; ssh-keygen -> openssh-keygen; hostapd template ->
# ttos-wifi-ap; networkctl/hostname -> systemd.
RDEPENDS:${PN} = "shadow openssh-keygen ttos-wifi-ap"

# Deploy the example onto the FAT boot partition too (see IMAGE_BOOT_FILES in the
# image recipe), so a freshly-flashed card already carries the template -- copy it
# to ttos-provision.conf, edit per car, boot. No round-trip to the build box.
# (Machine-specific deploy, so this recipe is no longer allarch.)
do_deploy() {
    install -d ${DEPLOYDIR}
    install -m 0644 ${WORKDIR}/ttos-provision.conf.example ${DEPLOYDIR}/ttos-provision.conf.example
}
addtask deploy after do_install before do_build

SYSTEMD_SERVICE:${PN} = "ttos-provision.service"
SYSTEMD_AUTO_ENABLE = "enable"

do_install() {
    install -d ${D}${bindir}
    install -m 0755 ${WORKDIR}/ttos-provision.sh ${D}${bindir}/ttos-provision

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/ttos-provision.service ${D}${systemd_system_unitdir}/ttos-provision.service

    # Reference copy on the rootfs. Ops copy this onto the FAT partition as
    # ttos-provision.conf and edit it per car (see README).
    install -d ${D}${datadir}/ttos
    install -m 0644 ${WORKDIR}/ttos-provision.conf.example ${D}${datadir}/ttos/ttos-provision.conf.example
}

FILES:${PN} += " \
    ${bindir}/ttos-provision \
    ${systemd_system_unitdir}/ttos-provision.service \
    ${datadir}/ttos/ttos-provision.conf.example \
"
