# TinyTrekOS CTF — System Overview

**Status as of 2026-08-02.** Baseline vehicle platform is **complete and working**:
boots reliably, both CAN buses up, WiFi AP, operator dashboard, and the dashboard
drives a real car. **No challenge logic exists yet** — that was deliberate. This
document is the handoff for designing the CTF challenges.

Repo: `~/ttos-ctf` (ported from the public `sixvolts/TinyTrek`, not a fork).

---

## 1. Hardware

| Part | Detail |
|---|---|
| Compute | Raspberry Pi 4, 2 GB |
| CAN | Waveshare **2-CH CAN FD HAT Rev2.1**, "Mode A" (two independent SPI buses), MCP2518FD ×2, 40 MHz oscillator |
| Motor nodes ×2 | **Adafruit QT Py SAMD21** + MCP2515 (`CAN.setPins(3, 0)`), stepper drivers |
| BMS node | **Adafruit Feather RP2040 CAN** (built-in MCP2518FD), drives a 12 V relay, reads pack voltage |
| Battery | 3S pack. 8.4 V = 0 %, 12.3 V = 100 %. Sense = 120 kΩ/40.2 kΩ divider (0.5 %) → BMS `A1` |

Pi ↔ nodes are connected **only** by CAN. There is no other link.

---

## 2. CAN buses — read this carefully

Two buses, and **the naming is inverted relative to the original design doc**:

| Interface | Config | What is actually on it |
|---|---|---|
| `can0` | **CAN FD**, 500 kbit arbitration / 1 Mbit data | **DIAG bus.** No permanent nodes — terminates at the contestant side tap. Writes fail `ENOBUFS` until an adapter is plugged in (a lone node gets no ACK), which is correct for a diagnostic port. |
| `can1` | **Classic CAN 2.0, 500 kbit** | **DRIVE bus** — both motors + BMS. Verified 2026-08-01. |

**Consequences for challenge design:**

- Anything that drives the car must use **`can1`**. `TTOS_DASH_DRIVE=can1`.
- **The DRIVE bus is classic CAN 2.0 and must stay that way.** Every node on it is
  classic-only — the motor nodes are MCP2515, which has no FD support at all. An FD
  frame there is a form error: the nodes go error-passive, then bus-off, and
  propulsion stops. `can1` was briefly configured `FDMode=yes` with a 1 Mbit data
  rate, which bought nothing (no node could use it) while leaving the Pi able to
  transmit a frame that would take the drivetrain offline. Removed 2026-08-02 — with
  FD off, the controller **cannot** emit one even by mistake. `ttos-selftest` fails
  if `can1` ever comes up in FD mode again.
- The gateway still forwards classic frames only (no `-X`), which is now redundant
  protection rather than the sole line of defence.
- Interface naming is pinned by `10-can0.link` / `10-can1.link` (matching SPI paths
  `platform-fe204000.spi*` → `can0`, `platform-fe215080.spi*` → `can1`) so names are
  stable across boots. Swapping those two `Path=` tokens would rename the buses if
  you'd rather match the original doc.

### Protocol (all on the drive bus)

| ID | Dir | Payload | Rate |
|---|---|---|---|
| `0x100` | Pi → nodes | `[seq][flags]` — **heartbeat**, "Pi alive and in control" | 5 Hz |
| `0x111` | Pi → L motor | `[steps:uint32 BE][dir][rpm]` | on demand (~6.7 Hz while held) |
| `0x113` | Pi → R motor | same | same |
| `0x115` | Pi → BMS | `[0x01]` = 12 V on, `[0x02]` = off | on demand |
| `0x116` | BMS → all | `[v_hi][v_lo][pwr][flags]` — **beacon**: pack mV (uint16 BE), `pwr` `0x01`=rail on/`0x02`=off (**actual** state), `flags` bit0 = LVC latched | 5 Hz |

Motor semantics: `dir` `0x01`=forward, `0x02`=reverse, `0x00`=stop/clear. `steps` is
**added** to a capped buffer (`STEPS_MAX`=250) that the node drains as it steps, so
motion tolerates dropped frames. `rpm` sets speed (100 straight / 50 turns).
Differential drive: forward = L01/R01, turns use opposite directions.

**No authentication on any of this.** Any device on the bus can command motion or
cut power. That is the intended CTF attack surface.

---

## 3. Status LEDs (system-state visibility)

Every node's onboard NeoPixel reports the health of the **whole chain**.

**Motor nodes** (QT Py SAMD21, `PIN_NEOPIXEL`=11, no power-gate pin):

| LED | Meaning |
|---|---|
| 🟢 solid green | READY — heartbeat + 12 V both good, idle |
| 🟡 solid yellow | ACTIVE — propulsion running |
| 🔴 double-blink | no Pi heartbeat (12 V fine) |
| 🔴 regular blink | no 12 V active (heartbeat fine) |
| 🔴 solid | both missing; also the power-on state → node isn't receiving CAN at all |

**BMS node** (Feather RP2040 CAN): blink = 12 V relay (solid off / slow blink on),
colour = battery band (green ≥50 %, amber 25–50 %, red <25 %), **fast-blink red** =
low-voltage cutoff latched.

Note: the heartbeat only transmits when driving is **enabled**, so a read-only
(provisioned) car's motors double-blink red by design — not a fault.

---

## 4. Networking

| | |
|---|---|
| AP subnet | **`192.168.244.0/24`**, car at **`.1`**, DHCP pool `.10–.59` |
| Default route | **Not offered** (`EmitRouter=no`) — joining the car AP won't hijack a client's internet |
| Provisioned AP | Per-car SSID + WPA2-PSK, 5 GHz non-DFS channel, low TX power (500 mBm ≈ 5 dBm, ~1–2 m range) |
| Factory AP | **`TTOS-TEST`**, **open, no password** (unprovisioned cars only) |
| Ethernet | `eth0`/`end0`, DHCP + link-local; optional static via provisioning |

**Listening services:** dashboard **TCP 80** (on the AP address), **sshd TCP 22**
(socket-activated; password auth on, root login denied).

> `socketcand` (TCP 29536, bridged both buses) was **removed 2026-08-02**. It was an
> unauthenticated network path to the DRIVE bus, and it would not have worked as a
> contestant-facing diagnostic path anyway: it is only a front-end to the Pi's own
> `can0` controller, so with no physical tap attached there is no second node to ACK
> and nothing transmits. Diagnostic access is the **physical side tap only**.

> Chrome force-upgrades `http://192.168.244.1` to HTTPS and won't fall back. Use
> Safari/Firefox, or disable "Always use secure connections". Port 80 was a
> deliberate choice (clean URL).

---

## 5. Operator dashboard

Go service (`ttos-dashboard`), stdlib-only, static aarch64 binary, embedded UI.

- `GET /` — operator console UI (TU navy + gold, Shojumaru logo)
- `GET /events` — SSE stream: `frame` / `log` / `status` events
- `GET /api/info` — ifaces, car id, client keepalive interval
- `POST /api/control` — `{"cmd": "forward|back|left|right|cw|ccw|stop|coast"}`

UI: tabbed console (Debug + one tab per bus, live frames with decoded notes),
indicator panel (12V ACTIVE, battery %, sensors, WiFi/Ethernet), control pad
(hold-to-drive; STOP cuts the 12 V rail).

**Safety gate:** `TTOS_DASH_DRIVE` (in `/etc/default/ttos-dashboard`) is **empty by
default** = read-only; the UI validates and logs but transmits nothing. Set to
`can1` + restart to enable driving. Factory mode sets it automatically.

Battery gauge applies a **load-compensation offset** (`+1000 mV` default): the pack
reads ~1 V lower under the Pi's steady load, so the gauge adds it back before
mapping 8.4–12.3 V → 0–100 %. The 12 V indicator reflects the **BMS's real reported
state**, not the commanded one (so it tells the truth during an LVC cutoff).

All tuning is live via `/etc/default/ttos-dashboard` (rpm, keepalive, step chunk,
battery range/offset, heartbeat period) — no rebuild.

---

## 6. Boot, provisioning, and factory mode

One image for the whole fleet; per-car identity applied on first boot from
`/boot/ttos-provision.conf` (FAT partition), then **consumed and wiped**.

Provisioning sets: hostname, car ID, WPA2 SSID/PSK/channel/country/TX power,
`ttos` password (crypt hash — plaintext is rejected), optional SSH authorized key,
optional static Ethernet, and **regenerates per-car SSH host keys**. Marker:
`/etc/ttos/provisioned`. Log: `/etc/ttos/provision.log` (persistent, one line per
boot decision). Malformed file = **fail loud** (banner on console + `/etc/issue`),
and the file is left in place to be fixed in the field.

**Factory / test mode** — when *no* provisioning file is present:
open `TTOS-TEST` AP, driving enabled (`can1`), console login **`ttos` / `ttos`**,
loud banner. Marker `/etc/ttos/factory`. Provisioning overwrites all of it and
re-locks the car. Intended for bench hardware bring-up only.

**Rootfs auto-expand** (`ttos-growfs`) runs *after* provisioning and only on a
provisioned car; grows the root partition + ext4 to fill the SD card, once.

---

## 7. Security posture (baseline — pre-challenge)

- `root` **locked**, no login on any path. Ops log in as `ttos` and `sudo` (wheel).
- U-Boot **removed** — an interruptible bootloader prompt on the serial console is
  a password-free root shell.
- Serial ops console on the **GPIO UART** (`ttyAMA0`, internal header), password
  gated. Plus a USB CDC-ACM console (`g_serial` → `ttyGS0`) for laptop access.
- SSH: `PermitRootLogin no`, password auth **on**, per-car host keys.
- Provisioning secrets: `/etc/ttos/provision.src` mode 600; FAT copy shredded.

**Deliberate weaknesses available as challenge material:** unauthenticated CAN,
open factory AP, password auth on SSH, no bus-level access control, `12V ACTIVE` as
a physical/observable state.

---

## 8. Build

```bash
cd ~/ttos-ctf && ./build.sh            # kas + bitbake in a pinned container
```

Yocto **scarthgap**; layers `poky`, `meta-openembedded`, `meta-raspberrypi`,
`meta-tt-ctf` (ours). Machine `ttos-ctf-hw` (RPi4-64 derived). Output:
`~/yocto/tmp-glibc/deploy/images/ttos-ctf-hw/ttos-ctf-image-ttos-ctf-hw.rootfs.wic`
(~532 MB). Two images: `ttos-ctf-image` (production) and `ttos-ctf-image-dev`
(bench, debug-tweaks — **never** flash to competition cars).

Node firmware lives in `firmware/` with **vendored** libraries (`CAN`,
`Adafruit_NeoPixel`); built/flashed from a laptop via `firmware/build.sh` or the
Arduino IDE. FQBNs: motors `QT Py SAMD21`, BMS `rp2040:rp2040:adafruit_feather_can`.

On-device health check: **`sudo ttos-selftest`** (one command, covers boot, CAN,
AP, provisioning, accounts).

---

## 9. Gotchas that cost real time

- **Never order a `multi-user.target` unit `Before=sshd.socket`.** It creates a
  systemd **ordering cycle**; systemd breaks it by deleting a job at random, so a
  unit silently never runs — non-deterministically. This killed the AP on ~half of
  boots. Diagnostic: enabled unit at `inactive (dead)` with **zero** `journalctl -u`
  entries → check `journalctl -b | grep -i 'ordering cycle'` first.
- Minimal rootfs is **busybox**: `head`/`tail` need `-n N`; `ip`/`ss` are in `/sbin`
  (not in the `ttos` PATH — use `sudo`).
- journald is **volatile** — only the current boot is visible. Consider
  `Storage=persistent` before chasing another intermittent boot bug.
- The Pi has **no RTC**; timestamps are wrong until NTP syncs (there's no internet
  on the AP), so log dates can read months off.
- Don't let automation rewrite operator config every boot — factory mode used to
  clobber `TTOS_DASH_DRIVE` on each boot and silently broke driving.

---

## 10. Open items

- **Bus naming**: drive bus is `can1` (FD-configured) while the doc says `can0`.
  Leave as-is / swap `.link` files / rewire — decide before challenges assume a layout.
- **Debug access for the AI agent**: arcana is wired-only and can't join the car AP.
  Options discussed, none implemented: Ethernet + SSH (cleanest), persistent
  journal, USB serial to arcana, on-device support-bundle command.
- **Left motor stutter**: intermittent, survived several drive-model rewrites;
  suspected node/wiring rather than software. The new status LEDs are intended to
  localise it.
- **Sensors**: U-Sonic / Camera indicators are placeholders — no telemetry defined.
- **No challenge content exists yet.**
