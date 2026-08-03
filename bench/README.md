# TTOS CTF bench rig — as built

Companion to `ttos-ctf-bench-rig.md` (the design brief) and `CHALLENGE-PLAN.md`.
This file records what is actually wired and running on `arcana`, and **where the
brief is wrong**. Where the two disagree, this file is the one that was measured.

Built and verified 2026-08-03.

---

## Quick start

```sh
sudo ./bench-up.sh          # bring both buses up, prove the role mapping
./harness-selftest.py       # prove the harness can be trusted  <-- do not skip
./bench-status.sh           # what is the DUT actually running
./push-dashboard.sh         # rebuild + swap the Go binary, ~2 s
```

---

## Topology as wired

| Role | arcana iface | USB port | Config | DUT side |
|---|---|---|---|---|
| DRIVE | `ttdrive` | `3-6.3` | classic 500 k | DUT `can1` |
| DIAG | `ttdiag` | `3-6.4` | FD 500 k / 1 M | DUT `can0` |

DUT: `192.168.4.133`, user `ttos`, unprovisioned (factory mode, password `ttos`).
Key auth is installed from `arcana`, so scripts run without a password prompt;
`sudo` on the DUT is fed the factory password on stdin rather than being given a
NOPASSWD drop-in, so the DUT keeps stock privilege config.

Adapters are two **PEAK PCAN-USB FD** — both FD-capable, so the brief's "this
leaves both table loaners free" holds.

---

## Corrections to the brief

### 1. `CAN_RAW_LOOPBACK` on the observer does nothing (§3)

The brief calls this the cleaner of its two fixes for the loopback trap. It does
not work. Measured here:

```
LOOPBACK=0 on the RECEIVER  -> receiver still saw the injected frame
LOOPBACK=0 on the SENDER    -> receiver did not
```

The option governs whether frames **that socket transmits** are echoed to other
local sockets. On a receive-only socket it suppresses the echo of frames it never
sends, i.e. nothing.

Worse, an observer built to the brief still **passes the brief's own self-test**,
because that test only asks whether the injected frame was absent — and a socket
that receives nothing at all answers yes. So the prescribed check certifies the
broken implementation.

`ttoscan.Observer` filters on the receive side instead: the kernel sets
`MSG_DONTROUTE` in `recvmsg` `msg_flags` for any frame that came from a local
socket, and leaves it clear for frames off the wire (measured: `0x4` for injected,
`0x0` for the DUT's heartbeat). This beats both of the brief's options — sender-side
suppression only covers injectors that opt in, so `cansend`, ad-hoc scripts and the
panel all still leak; frame bookkeeping needs every transmitter to report to a
ledger. `MSG_DONTROUTE` is the kernel's own statement of provenance and needs
cooperation from nobody.

`harness-selftest.py` therefore adds **positive controls** (T1, T3) around the
negative check (T2). T2 alone is unfalsifiable.

### 2. Adapters are wired opposite to the brief's table (§1)

The brief has `can0`=DIAG, `can1`=DRIVE. Here kernel `can0` was on the DRIVE bus.
Renumbering would only move the trap, so the adapters are pinned to **role names**
(`ttdrive`/`ttdiag`) by udev. A script that says "observe `ttdiag`" cannot be
silently backwards.

The DUT keeps its own `can0`/`can1` names — those are baked into shipped `.network`
files and dashboard config. The two sides never have to agree, which is the point.

### 3. Pin by USB port, not adapter serial (§2)

PCAN-USB FD publishes no USB `iSerial`; there is no serial to match on. The rules
key on `ID_PATH` (physical port). **The role follows the port, not the adapter** —
move a cable and the roles move with it. `bench-up.sh` re-verifies the mapping
against the DUT's 0x100 heartbeat on every run, so a mis-plug aborts instead of
inverting every result.

### 4. `cmd/ctfd` / `ttos-ctfd` do not exist (§5)

There is no separate CTF daemon. The challenge layer lives inside the dashboard:
`cmd/dashboard/ctf.go` and `cmd/dashboard/uds.go`. Building `./cmd/ctfd` fails
loudly (fine); restarting a nonexistent `ttos-ctfd` succeeds *quietly* and leaves
the old code running (not fine). `push-dashboard.sh` targets `ttos-dashboard`.

### 5. `install(1)` is not on the DUT

busybox is built without it. `push-dashboard.sh` uses `cp` + `chmod` + `mv`, which
keeps the atomic-rename property that matters (the running process holds its old
inode; a half-copied binary can never be exec'd).

---

## Deliberate choice: the DRIVE adapter is CLASSIC

The real drive-bus nodes are MCP2515s with no FD support at all. Configuring
`ttdrive` classic makes it fail the way they do — a genuine FD frame becomes a form
error rather than data. That turns the adapter into a **canary** for the FD hazard
(matrix G3) instead of an FD-tolerant observer that would quietly absorb the exact
frame we are trying to prove never arrives.

`bench-up.sh` passes `fd off` explicitly and then re-reads the kernel's view to
confirm. This is not belt-and-braces: setting `bitrate` alone leaves whatever FD
state the interface last had, so an adapter previously used as DIAG comes back up
FD-capable while the script cheerfully reports `CLASSIC`. That happened here on the
first run, and the only outward sign was a `dbitrate` line in `bench-status.sh`.

Set `DRIVE_FD=1` only to *capture a stray FD frame's contents*, never to run G3.

---

## Known drift: the DUT is one image behind

`bench-status.sh` currently reports:

```
!! DRIVE-BUS FD MODE DIFFERS:  source FDMode=yes:0   DUT FDMode=yes:1
```

The DUT predates `112e243` (drive bus back to classic CAN 2.0). Its `can1` is still
`FDMode=yes`, so **it can still transmit CAN FD onto the drive bus and G3 cannot be
closed on it**. Everything else on it is current: gateway policy live with exactly
two rules (`can1 -> can0` for `7D1`/`7D2`, nothing inbound), dashboard active, UDS
server bound to the DIAG bus.

`.network` files ship in the image. `push-dashboard.sh` cannot fix this — it needs
a flash of
`ttos-ctf-image-ttos-ctf-hw.rootfs-20260802222942.wic`
(sha256 `ca137190c205bdbccfb18637c8cfba967d3167ccb3bc6c3c15b06dd127d9c8fe`).

**This is the general hazard of the fast loop.** The binary tracks the source tree
continuously while the platform stays frozen at the last flash, so the DUT drifts
into a configuration that exists nowhere in git. Run `bench-status.sh` whenever a
result stops making sense.

---

## Not yet built

- **Vehicle emulator** — motor nodes + BMS on DRIVE (brief §4). Nothing currently
  answers drive commands; the DUT is talking to an empty bus.
- **Test matrix runner** — brief §6 (G1–G6, D1–D11, C1–C8, P1–P8).
- **Remote power control** — no `uhubctl`, no switchable hub. A wedged DUT still
  needs a human, which caps unattended iteration.
- **Serial console** — nothing on `/dev/serial/by-id`. When networking is what
  broke, there is currently no way in.
- **`rpiboot` reflash path** — not installed; flashing still means moving the card.
- **Static DUT address** — currently DHCP at `192.168.4.133`.

## What the rig still cannot tell you

Unchanged from brief §8, and worth re-reading before trusting a green suite: node
firmware correctness, stepper/drivetrain behaviour (including the known left-motor
stutter), real bus timing, RF, physical containment. The emulator will test the
*specification*; one real car still has to close the loop before the fleet is
flashed.
