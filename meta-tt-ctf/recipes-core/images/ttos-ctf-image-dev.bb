require ttos-ctf-image.bb

DESCRIPTION = "TinyTrekOS CTF BENCH image -- debug-tweaks enabled. DO NOT FLASH TO CARS (§4.8)"

# Bench convenience: SDK/debug tooling + debug-tweaks (empty root password,
# passwordless login, root SSH). Explicitly NOT for competition cars.
EXTRA_IMAGE_FEATURES = "debug-tweaks tools-sdk tools-debug"

# Bench needs a usable root; do not lock it (overrides the production locks).
EXTRA_USERS_PARAMS = "\
    groupadd -f wheel; \
    useradd -m -G wheel -s /bin/bash ttos; \
"

# Make it impossible to confuse with production (§4.8): distinct hostname + banner.
ROOTFS_POSTPROCESS_COMMAND += "ttos_bench_marker; "
ttos_bench_marker() {
    echo "ttos-ctf-bench" > ${IMAGE_ROOTFS}${sysconfdir}/hostname
    echo "TinyTrekOS-CTF (BENCH / debug -- NOT FOR COMPETITION)" > ${IMAGE_ROOTFS}${sysconfdir}/ttos-variant
    printf '\n*** BENCH IMAGE: debug-tweaks enabled (empty root password). NOT for competition cars. ***\n\n' > ${IMAGE_ROOTFS}${sysconfdir}/issue
}
