SUMMARY = "socketcand -- bridges SocketCAN to a TCP/ASCII network protocol"
DESCRIPTION = "Daemon that provides access to CAN interfaces on a machine over the \
network, used by the CTF control panel (Flask/SocketIO) and remote CAN tooling. \
No recipe ships in meta-networking for scarthgap (brief §5.7 [VERIFY]); this is a \
local recipe pinned to release 0.6.1."
HOMEPAGE = "https://github.com/linux-can/socketcand"
SECTION = "net"

# Dual-licensed BSD-3-Clause OR GPL-2.0 (per the source file header; there is no
# standalone licence file in the tree).
LICENSE = "BSD-3-Clause | GPL-2.0-only"
LIC_FILES_CHKSUM = "file://socketcand.c;beginline=1;endline=45;md5=623ba5a286a1a5158217d6308355bdc7"

SRC_URI = " \
    git://github.com/linux-can/socketcand.git;protocol=https;branch=master \
    file://socketcand.service \
"
# Release 0.6.1. Pinned SRCREV for reproducibility (§7).
SRCREV = "46c5ce67a70f55af74768ba70fc022cb12e1b51e"

S = "${WORKDIR}/git"

DEPENDS = "libconfig"

# The upstream Makefile.in is hand-written and only supports in-tree builds
# (install copies from $(srcdir)); autotools-brokensep sets B=S to match.
inherit autotools-brokensep systemd

# We are systemd-only; do not generate the SysV init.d script.
EXTRA_OECONF = "--disable-init-script"

# socketcand 0.6.1 predates two toolchain-era changes:
#  -DSIOCGSTAMP=0x8906 : glibc >= 2.30 no longer exposes SIOCGSTAMP via
#      <sys/ioctl.h>; 0x8906 is the asm-generic value used on aarch64 (our target).
#  -fcommon           : GCC >= 10 defaults to -fno-common, turning the globals this
#      codebase declares in headers without 'extern' (tv, readfds, ifr, ...) into
#      "multiple definition" link errors. -fcommon restores the merging behaviour.
# Configure folds ${CFLAGS} into the Makefile's @CFLAGS@, so these reach compile+link.
CFLAGS:append = " -DSIOCGSTAMP=0x8906 -fcommon"

# Service shipped but NOT auto-enabled: on a bench build with no CAN HAT it would
# crash-loop. Enable per deployment (systemctl enable socketcand) once candrive/candiag exist.
SYSTEMD_SERVICE:${PN} = "socketcand.service"
SYSTEMD_AUTO_ENABLE = "disable"

do_install:append() {
    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/socketcand.service ${D}${systemd_system_unitdir}/socketcand.service
}
