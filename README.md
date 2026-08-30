# ttos-ctf — TinyTrekOS

A small autonomous-ish car built to be hacked, and the Yocto Linux distribution
that runs it. Eight identical vehicles, each a genuine automotive network in
miniature: two CAN buses, three microcontroller nodes, a battery manager that
watches the drivetrain, and an operator panel served over the car's own WiFi that
starts out locked and opens as you solve it.

It was built for an authorized, educational capture-the-flag event. The three
challenges are the real thing rather than a simulation of one — a UDS diagnostic
server on CAN FD, an AUTOSAR-style end-to-end CRC protecting the drive bus, and
an in-kernel CAN gateway between the two — which is the point. What a team learns
breaking this car transfers to a real one.

> **Authorized use only.** Everything here targets hardware you own and a network
> with nothing else on it. Applying any of it to a vehicle you do not own, or to
> one on a public road, is neither the intent nor defensible.

> **Spoilers.** This repository is the complete build *including the answers*.
> [`WALKTHROUGH.md`](WALKTHROUGH.md) is a full solution guide, [`ARCHITECTURE.md`](ARCHITECTURE.md)
> §6 explains every mechanism, and the live flag codes appear in both. If you are
> planning to *play* one of these cars, read [`BOARD.md`](BOARD.md) and stop there.

---

## Hardware

| Part | Detail |
|---|---|
| Compute | Raspberry Pi 4, 2 GB |
| CAN | Waveshare 2-CH CAN FD HAT Rev2.1, "Mode A" (two independent SPI buses), MCP2518FD ×2 |
| Motor nodes ×2 | Adafruit QT Py SAMD21 + MCP2515, stepper drivers |
| BMS node | Adafruit Feather RP2040 CAN (built-in MCP2518FD), 12 V relay, pack-voltage sense |
| Battery | 3S pack, 8.4 V–12.3 V |

Two buses: **`can0` is DIAG** (CAN FD, 500 kbit / 1 Mbit, terminating at the
contestant's diagnostic tap) and **`can1` is DRIVE** (classic CAN 2.0, 500 kbit,
both motors plus the BMS). That naming is inverted relative to the original design
notes and it matters — see [SYSTEM-OVERVIEW.md §2](SYSTEM-OVERVIEW.md) before you
touch anything bus-related. The Pi reaches the nodes over CAN and nothing else.

## Documentation

Read in roughly this order. [`ARCHITECTURE.md`](ARCHITECTURE.md) is the source of
truth — where an older document disagrees with it, it wins.

| Document | What it covers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) | **Source of truth.** Physical layout, full CAN message map, where every secret lives, the gateway, all three challenges as built, detection and judging, panel tiers |
| [SYSTEM-OVERVIEW.md](SYSTEM-OVERVIEW.md) | The platform beneath the challenges: hardware, buses, networking, dashboard, boot and provisioning, security posture |
| [BOARD.md](BOARD.md) | Contestant-facing board copy. Safe to print or post — reveals no solutions |
| [WALKTHROUGH.md](WALKTHROUGH.md) | **Answer key.** Solving all three by hand on a real car, with the reasoning at each step |
| [THIRD-PARTY.md](THIRD-PARTY.md) | Vendored components and their licenses |
| [firmware/README.md](firmware/README.md) | The three CAN node sketches |
| [firmware/FLASHING.md](firmware/FLASHING.md) | Flashing nodes, per-car and bench modes |
| [provisioning/README.md](provisioning/README.md) | Fleet data model — how eight cars get eight identities from one image |
| [bench/README.md](bench/README.md) | The development bench rig as actually wired, and the test suite |

## Layout

```
ttos-ctf/
  meta-tt-ctf/            the Yocto layer — distro, machine, image, and every app
    recipes-apps/         ttos-dashboard (Go): panel, UDS server, CTF logic
    recipes-provision/    first-boot per-car identity
    recipes-support/      ttos-selftest, ttos-bringup, operator tooling
    recipes-core/         image definitions, systemd units, network config
    recipes-kernel/       CAN kconfig, mcp251xfd
  hardware/               build it yourself — BOM, board fab files, 3D models, schematics, build guide
  firmware/               Arduino sketches for the two motor nodes and the BMS
  bench/                  bench rig control, vehicle emulator, test suite
  tools/                  operator tooling
    ttos-ctf-tool.py      drives a car over CAN — the whole challenge chain
    linux-kit/            self-contained kit for an operator laptop
  provisioning/           fleet generation (data itself is gitignored — see below)
  kas/                    build configs
  docker/                 pinned build container
  build.sh                container build wrapper
  apps/dashboard          → symlink into meta-tt-ctf, for convenient editing
```

## Build

Needs Docker and a fast scratch directory. Upstream Yocto layers (`poky`,
`meta-openembedded`, `meta-raspberrypi`) are cloned into the repo root by `kas`
at build time and are deliberately not committed — `kas/ttos-ctf.yml` is the
source of truth for their revisions.

```bash
tmux new -s ttos                # a first build is long; sessions drop
./build.sh --build              # build the container, then the production image
./build.sh kas/ttos-ctf-dev.yml # bench/dev image instead
```

Build directories (`TMPDIR`, `SSTATE_DIR`, `DL_DIR`) live under `$TTOS_YOCTO_DIR`
(default `$HOME/yocto`), **outside** the source tree, so `rm -rf build/` does not
destroy sstate or downloads. The image lands in
`$TTOS_YOCTO_DIR/tmp-glibc/deploy/images/ttos-ctf-hw/`; flash it with
`bmaptool copy`.

## Bringing up a car

One image serves the whole fleet; per-car identity is applied on first boot from
`ttos-provision.conf` on the FAT boot partition — hostname, car id, WiFi
credentials, console password hash, challenge constants. Provisioning is
idempotent, regenerates per-car SSH host keys, then wipes the file off the FAT
partition. A missing or malformed file makes the car fail loudly on the console
rather than boot with defaults.

An **unprovisioned** card boots into factory mode: a known AP, driving enabled,
no challenge gating — which is what `tools/ttos-bringup.sh` uses to verify a
freshly assembled car before it gets an identity.

The panel is at `http://192.168.244.1` once you join the car's AP. **Use Safari
or Firefox** — Chrome's "always use secure connections" upgrades that URL to
HTTPS, which is not served, and the panel looks dead when it is fine.

## Secrets

Per-car provisioning data — WiFi PSKs, console password hashes, CRC data IDs,
challenge codes — is generated into `provisioning/` and is **gitignored**. Only
the generator scripts are tracked; `render-fleet.py` rebuilds every derived
artifact deterministically from `fleet-table.csv`. Back that file up somewhere
off this repository: losing it means reflashing every node in the fleet with new
values.

The flag codes themselves *are* in this repository, in the walkthrough and the
architecture document, because the event they were built for is over.

## License

MIT — see [LICENSE](LICENSE). Vendored third-party components keep their own
licenses, some of them LGPL; see [THIRD-PARTY.md](THIRD-PARTY.md).
