# Default hostname deliberately signals an UNPROVISIONED car. First-boot
# provisioning (§5.6) overwrites /etc/hostname with the real per-car name
# (e.g. ttos-car-03). If you ever see this hostname on a competition car,
# provisioning did not run -- which §5.6 requires to fail loudly.
hostname = "ttos-ctf-unprovisioned"
