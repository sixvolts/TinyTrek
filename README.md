# ttos-ctf — TinyTrekOS CTF Baseline

A minimal, reliable Yocto image that boots the CTF vehicle hardware and brings up
both CAN interfaces and a WiFi AP. **No challenge logic** — that comes later against
this known-good base. Built to the *TinyTrekOS CTF Baseline Build Brief*.

This is a **new repository**, deliberately not forked from `sixvolts/TinyTrek`
(§3), so challenge design does not leak through commit history and the stale public
config is not inherited. The public repo was used only as a porting reference.

Target: **Raspberry Pi 4 (64-bit, 2 GB min)** + **Waveshare 2-CH CAN FD HAT Rev2.1**.

---

## Layout

```
ttos-ctf/
  kas/
    ttos-ctf.yml            # production build config (no secrets)
    ttos-ctf-dev.yml        # bench/dev image (debug-tweaks)
  meta-tt-ctf/              # the ported + rewritten layer
    conf/{distro,machine}/
    recipes-kernel/linux/   # §4.1 fix: CAN kconfig on linux-raspberrypi + mcp251xfd
    recipes-bsp/bootfiles/  # config.txt overlays (mcp251xfd, Mode A)
    recipes-core/systemd/   # can0/can1/wired/wlan-ap .network + .link
    recipes-core/images/    # ttos-ctf-image (prod) + -dev (bench)
    recipes-connectivity/   # ttos-wifi-ap (hostapd), openssh hardening
    recipes-provision/      # ttos-provision (first-boot §5.6)
    recipes-support/        # ttos-ops (sudoers), ntp, vim
    recipes-devtools/       # python3 websocket recipes (for later control panel)
  docker/Dockerfile         # pinned build container, parameterized UID/GID
  build.sh                  # container build wrapper (no SSH agent forwarding)
```

## Build

Prereqs: Docker, and a fast scratch dir for the build (Optane on arcana as briefed;
this repo defaults to `$HOME/yocto` — override with `TTOS_YOCTO_DIR`).

```bash
# Run under tmux (arcana sessions can drop, §6).
tmux new -s ttos
./build.sh --build            # builds the container, then the production image
# bench image:
./build.sh kas/ttos-ctf-dev.yml
```

Output image (`.wic.bz2` / `.wic.bmap`) lands under
`$TTOS_YOCTO_DIR/tmp-glibc/deploy/images/ttos-ctf-hw/` (poky appends the standard
`-glibc` `TCLIBCAPPEND` suffix to `TMPDIR`). Flash with `bmaptool copy`.

`TMPDIR`, `SSTATE_DIR`, `DL_DIR` live under the mounted `/yocto` volume, **outside**
the build tree, so `rm -rf build/` does not nuke sstate/downloads (§6).

## First-boot provisioning (§5.6)

One image serves the whole fleet; per-car values are applied on first boot from a
plain-text file on the **FAT boot partition**.

1. Copy `meta-tt-ctf/recipes-provision/ttos-provision/files/ttos-provision.conf.example`
   onto the card's FAT partition as **`ttos-provision.conf`** (also installed on the
   rootfs at `/usr/share/ttos/`).
2. Edit per car: hostname, car id, WiFi SSID/PSK/channel/country/txpower, and the
   **serial console password *hash*** (never plaintext — generate with
   `openssl passwd -6 '...'` or `mkpasswd -m yescrypt '...'`).
3. Boot. Provisioning is idempotent (marker `/etc/ttos/provisioned`), regenerates
   per-car SSH host keys, then **wipes the file off the FAT partition** and moves it
   into the rootfs. A missing/malformed file makes the car **fail loudly** on the
   console instead of booting with defaults.

Login: root is **locked**; log in as **`ttos`** (serial or SSH) with the provisioned
password, `sudo` for privilege. Root SSH is refused.

## Operator console (`ttos-dashboard`)

Go service serving a live CAN console on the WiFi AP at **`http://192.168.4.1`**
(port 80). Join the car's AP, then browse to it. It monitors `can0`/`can1`, decodes
the reused TinyTrek protocol (`0x111`/`0x113` motor, `0x115` BMS/12V), and has a
control pad + indicators (battery/sensors/network/12V).

- **Browser note:** use **Safari or Firefox**. Chrome with "Always use secure
  connections" upgrades `http://192.168.4.1` to HTTPS (port 443, not served) and
  won't fall back, so it shows `ERR_CONNECTION_REFUSED` even though the console is
  fine. Fix in Chrome: `chrome://settings/security` → turn off "Always use secure
  connections". `curl http://192.168.4.1/` on the car always works.
- **Driving is gated off by default** (`TTOS_DASH_DRIVE=` empty in
  `/etc/default/ttos-dashboard`): the control buttons validate + log but do **not**
  transmit. To enable live driving, set `TTOS_DASH_DRIVE=can0` and
  `systemctl restart ttos-dashboard` (journal: "control is LIVE"). First live test:
  wheels-off, STOP ready.

## Verification on real hardware (maps to brief §7)

```bash
zcat /proc/config.gz | grep -E 'CAN_GW|MCP251XFD'   # §4.1 kconfig actually applied
dmesg | grep mcp251xfd                              # both controllers probe
ip -details link show can0                          # classic CAN, 500k, ERROR-ACTIVE
ip -details link show can1                          # CAN FD, 500k/1M
cangw -L                                            # go/no-go for §4.1
```

## Decisions taken (defaults, per §0 "record every deviation")

- **U-Boot dropped** (`RPI_USE_U_BOOT="0"`, §5.5 opt 1) — no interruptible bootloader
  prompt on the console (that would be a password-free root shell).
- **Serial console = GPIO UART (ttyAMA0)**, internal, not a contestant-facing USB
  port (§5.5 DECIDE). `disable-bt` frees the PL011 UART.
- **CAN HAT = Mode A** (factory-default dual-SPI). Overlays taken verbatim from the
  Waveshare wiki; oscillator left at the overlay's 40 MHz default (verified in the
  overlay source, matches the HAT crystal).
- **FD data rate = 1 Mbit/s** (not 2M) per decision — no reason to push the bus.
- **`meta-lts-mixins` removed** (§4.9) — it backports the linux-yocto kernel; we use
  the meta-raspberrypi kernel, so it added nothing.
- **Ethernet**: DHCP + link-local for a predictable, always-reachable address;
  per-car static override available via provisioning.

## Deviations from the brief's environment

- **No Optane on the current host.** The brief (§6) assumes arcana's 800 GB Optane
  P5800X; the box this was built on has a single 745 GB NVMe. Build dirs default under
  `$HOME/yocto` on that NVMe. Set `TTOS_YOCTO_DIR` to the Optane mount on arcana.

## Open items still requiring real hardware / a running system

- **[VERIFY-ON-BOARD]** HAT is jumpered/resistored for Mode A; confirm probe with
  `dmesg | grep mcp251xfd`.
- **[VERIFY-ON-BOARD]** Which physical screw terminal is the *internal* vehicle bus,
  and the exact `Path=` token for the `.link` files. `can0` **must** be the internal
  (classic) bus — see `10-can0.link` / `10-can1.link`. Derive the token with
  `udevadm info -q property -p /sys/class/net/can0 | grep ID_PATH`.
- **[VERIFY]** `CONFIG_CAN_GW` present in the *running* kernel and `cangw -L` works.
- **[VERIFY]** `socketcand` recipe availability in meta-networking (write a recipe if
  absent — see build notes).
- **[VERIFY]** CDC-ACM/serial console enumerates on macOS/Windows/Linux.
- **[DECIDE]** SD-card physical access: accept as out-of-scope tampering, or tamper
  tape + judge briefing. (Console hardening does not protect a pulled card.)
- Confirm loaner USB-Ethernet dongle count for the contestant fallback.
