SUMMARY = "TTOS CTF read-only CAN bus dashboard"
DESCRIPTION = "A dependency-free Go service that streams live CAN frames from can0/can1 \
to the browser over Server-Sent Events. Foundation for the CTF control panel. Built as \
a fully static binary (stdlib only, CGO disabled) so the recipe needs no module fetching."
HOMEPAGE = "local"
LICENSE = "MIT"
LIC_FILES_CHKSUM = "file://${COMMON_LICENSE_DIR}/MIT;md5=0835ade698e0bcf8506ecda2f7b4f302"

SRC_URI = " \
    file://src \
    file://ttos-dashboard.service \
    file://ttos-dashboard.default \
"

S = "${WORKDIR}/src"
GO_IMPORT = "ttos.local/dashboard"

inherit go systemd

# The module has no external dependencies, so there is nothing to fetch or vendor:
# skip the class's GOPATH scaffolding and build straight from the module in module
# mode, fully static (CGO_ENABLED=0) so no target C toolchain is needed either.
do_configure[noexec] = "1"

do_compile() {
    cd ${S}
    # GOPATH/GOCACHE/GOTMPDIR are set (as bitbake vars) by the go class and
    # normally created by its do_compile; since we override do_compile, create
    # them ourselves. Do NOT re-export them here -- ${VAR} in a task body is the
    # bitbake variable, so re-pointing them would diverge from the exported env.
    mkdir -p "${GOPATH}" "${GOCACHE}" "${GOTMPDIR}"
    export GO111MODULE="on"
    export GOFLAGS="-mod=mod"
    export GOPROXY="off"
    export CGO_ENABLED="0"
    export GOOS="linux"
    export GOARCH="${TARGET_GOARCH}"
    ${GO} build -trimpath -o ${B}/ttos-dashboard ./cmd/dashboard
}

do_install() {
    install -d ${D}${bindir}
    install -m 0755 ${B}/ttos-dashboard ${D}${bindir}/ttos-dashboard

    install -d ${D}${sysconfdir}/default
    install -m 0644 ${WORKDIR}/ttos-dashboard.default ${D}${sysconfdir}/default/ttos-dashboard

    install -d ${D}${systemd_system_unitdir}
    install -m 0644 ${WORKDIR}/ttos-dashboard.service ${D}${systemd_system_unitdir}/ttos-dashboard.service
}

SYSTEMD_SERVICE:${PN} = "ttos-dashboard.service"
SYSTEMD_AUTO_ENABLE = "enable"

FILES:${PN} += " \
    ${bindir}/ttos-dashboard \
    ${sysconfdir}/default/ttos-dashboard \
    ${systemd_system_unitdir}/ttos-dashboard.service \
"

# Static Go binary: Go doesn't consume the toolchain LDFLAGS, and there's no
# dynamic linkage to check.
INSANE_SKIP:${PN} += "ldflags"
