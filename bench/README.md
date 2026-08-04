# TTOS CTF bench rig — as built

Companion to `ttos-ctf-bench-rig.md` (the design brief) and `ARCHITECTURE.md`.
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
./push-config.sh            # sync bus config, gateway policy, selftest, units
```

---

## Topology as wired

| Role | arcana iface | USB port | Config | DUT side |
|---|---|---|---|---|
| DRIVE | `ttdrive` | `3-6.3` | classic 500 k | DUT `candrive` |
| DIAG | `ttdiag` | `3-6.4` | FD 500 k / 1 M | DUT `candiag` |
| DIAG (car) | `ttcardiag` | `0a:00.3-usb-0:3` | FD 500 k / 1 M | car 02 diagnostic port |

The DUT names its own interfaces by role too, from `10-ttos-can.rules` in the
image — `candrive` is the HAT's CAN0 terminal (`spi0.0`), `candiag` is CAN1
(`spi1.0`). Both sides therefore read as roles end to end, and neither depends on
kernel probe order.

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

### 2. Adapters are pinned to role names, not numbers (§1)

A script that says "observe `ttdiag`" cannot be silently backwards, whatever the
cabling is. That paid for itself on 2026-08-03, when the bus roles turned out to be
the reverse of what the config claimed — DRIVE is the car's `can0`, not `can1` —
and correcting the whole rig took two lines and no cable moved.

**And then it bit, because the same commit edited these two lines as well.** The
car-side correction was right; the bench mapping was already right and got
inverted along with it. For a day, every bench observation was on the wrong wire:
the DUT looked silent on DRIVE while transmitting normally. Nothing failed loudly,
because a swapped pair makes negative assertions pass.

The rule that falls out of that: **which arcana USB port is cabled to which DUT
bus is a physical fact, and a role correction at the far end is never also a
correction here.** Verify this file against traffic, not against reasoning about
the other end — which is exactly what `bench-up.sh` does, and it catches this
inversion in 1.5 s. It simply had not been run in between.

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

## Updating the DUT without a card swap

```sh
./push-dashboard.sh          Go binary          ~2 s
./push-config.sh             platform files     ~10 s  (--dry-run to preview)
```

`push-config.sh` syncs the files a Go rebuild cannot touch — `candrive/candiag.network`,
the udev naming rule, the AP network file, the `cangw` policy and its unit,
`ttos-selftest`, `ttos-provision`, `ttos-dashboard.default` and its unit — straight
from the source tree, then reconfigures the interfaces they describe. It compares
by sha256 and pushes only what differs.

**CAN link parameters can only be set while the interface is DOWN**, and
`systemd-networkd` will not bounce a link just because its `.network` changed. So
`networkctl reload` alone leaves an FD interface FD forever and the push looks like
it did nothing. The script takes both links down first — and then verifies against
the kernel anyway, because `networkctl reconfigure` was measured here NOT to clear
FD state it did not set (the same trap as `bench-up.sh` on the host side). It falls
back to an explicit `ip link set candrive up type can bitrate 500000 fd off`.

Interestingly, a **reboot** applies the file cleanly — the interface comes up
classic from the driver default and networkd only ever adds `BitRate`. So the
forced fallback matters for live iteration, not for a freshly booted car.

Still needs a real flash: kernel, device-tree overlays, layer/recipe changes,
package sets, and anything on the FAT boot partition. And this makes the *files in
the manifest* match — not the whole image. **Flash the real `.wic` and re-run the
suite before any milestone or go/no-go**; treat config push as the iteration path,
not the sign-off.

## Test matrix status

| Group | Covered | Result |
|---|---|---|
| Gateway | **G1–G6 complete** | 5/5 |
| Diagnostic | **D1–D10 complete**; D11 needs a soak | 11/11 + 8/8 |
| Challenge | **C1–C4, C6–C8**; C5 needs the 30-minute soak | 8/8 + 6/6 |
| Panel | **P1–P8 complete** | 13/13 |
| Relay | R1–R9 (not in the brief; C3 needed them) | 9/9 |

```sh
./provision-bench.sh 01                 # stage car 01's identity (identity only)
./vehicle-emulator.py --car 01 &
./harness-selftest.py   4/4    harness isolation
./test-gateway.py       5/5    G1-G5 (re-run after a reboot = G6)
./test-detectors.py     8/8    C2/C3 rules
./test-uds.py --car 01  11/11  diagnostic server + the C1 pivot routine
```

`provision-bench.sh` stages `/etc/ttos/provision.src` only — it deliberately does
NOT run `ttos-provision`, which would also set the hostname, switch the AP to WPA2,
reset the drive interface to read-only and replace the `ttos` password with the
fleet hash, breaking the factory password every bench script feeds to `sudo -S`.
The DUT therefore still reports itself unprovisioned and self-test sections 1 and
10 still fail; that is expected. **It also means the provisioning path itself is
not under test here** — flash a real image and run `ttos-provision` properly before
the fleet is built.

G3 is only meaningful because the DRIVE adapter is classic **and** the DUT's
`candrive` is classic. Both halves matter: an FD-tolerant observer absorbs the frame, and an
FD-capable DUT can still emit one.

Watch for two traps when adding cases, both of which bit here on first run:
the drive bus legitimately carries the DUT's `0x100` heartbeat, so a bare
`collect()` counts normal traffic as a leak; and a negative assertion over a dead
bus passes for free, so pair it with a liveness check (`bus_is_live`).

## Previously-known drift, now closed

The DUT was one image behind (pre-`112e243`): the drive bus still `FDMode=yes`, so it could
transmit CAN FD onto the drive bus and **G3 could not be closed on it**. Fixed with
`push-config.sh` on 2026-08-03, verified by `bench-status.sh`, the DUT self-test
(`candrive is classic CAN 2.0`), and G3 passing. The gateway also survives three
consecutive self-test runs at 2 rules, which is the proof the old destructive
`cangw -F` is gone.

## First provisioned-car run (2026-08-03)

Image `…rootfs-20260803163625.wic` flashed, `ttos-provision.conf` (car 01) consumed
on first boot. `ttos-selftest`: **40 PASS / 0 FAIL / 3 SKIP** — sections 1 and 10
pass for the first time, and `push-config.sh` found *only* `ttos-provision`
differing from source, so the image matched the tree everywhere else.

Full suite against the provisioned car: harness 4/4, gateway 5/5, detectors 8/8,
UDS 11/11, challenge 8/8.

Two bugs that only a real provisioning run could surface:

**`install(1)` in `ttos-provision.sh`.** There is no `install` on this rootfs —
busybox is built without it. `install -d -m 700 /home/ttos/.ssh` failed, so the
directory was never created, the `authorized_keys` redirect failed too, and the key
was silently not deployed. Nothing else depends on it, so provisioning reported
success; the only symptom is that key auth does not work, found whenever someone
next logs in without a password. Now `mkdir -p` + `chmod`, both with `|| fail`.

This one is doubly annoying: the same fact is already written down as correction 5
above, from fixing `push-dashboard.sh` — the fact was known and never applied to
the rest of the tree. A `grep -rE 'install +-'` across everything that runs *on the
car* takes seconds and was not done. It is done now, and `timeout`/`getent` were
swept at the same time (the only other hits are comments).

**The self-test still asserted `provision.src` mode 600.** The DynamicUser fix
changed it to `640 root:ttos-secrets` and the guard was not updated with it, so the
first provisioned boot reported a failure for doing the right thing. The check now
verifies mode *and owner*: `640 root:root` is just as broken as `600` and looks
correct at a glance.

## Vehicle emulator

```sh
./vehicle-emulator.py --car 01     # two motor nodes + BMS on the DRIVE bus
./emu-ctl.sh status                # state + counters
./test-detectors.py                # 7 C2/C3 rule cases
```

Provisioned as **car 01** — Data IDs `L=0x60B0` / `R=0x045C`, codes `C2=2FQYWXDM`,
`C3=3CX5E77Z`. Per brief §7 the bench must stay one car: a mismatch presents as
universal silent CRC rejection, which is indistinguishable from a bus fault.
(`fleet-table.csv` is gitignored, so the emulator needs `provisioning/` present.)

Verified against the live DUT:

- Beacon reaches the dashboard end to end — `emu-ctl.sh vbat 9500` moved the SSE
  battery reading 100% → 53%, matching the computed 53.8%.
- Heartbeat arm mask decoded as `0x03` (both detectors armed on a locked car).
- Driving the DUT via `/api/control` fired `0x7D1` and `0x7D2` carrying car 01's
  real codes, **and both reached the DIAG bus through the DUT's `cangw` policy —
  matrix G5 passes.** That is the rule whose omission ships an unsolvable event.
- CRC: valid frame accepted, 5 corrupted frames and 1 frame keyed to car 02's Data
  ID all rejected with **zero** response frames (silent rejection; matrix C8).
- LVC latches after the 1.5 s debounce and clears above 9000 mV, rail staying off
  until an explicit `0x115`, matching `TinytrekBMS.ino`.

`crc8_j1850` checks out against the standard vector (`"123456789"` → `0x4B`), and
the equivalence-class result reproduces independently: 1, 2 and 3 captures all
leave exactly 256 candidate Data IDs.

### Findings from building it

**1. The brief's heartbeat requirement is wrong, and the real behaviour is worse.**
§4 says the emulator must "refuse motion when absent, matching real node behaviour."
`TinytrekLMotor.ino` does no such thing — `heartbeatOK()` and `powerOK()` feed
`updateLed()` and nothing else; the step-drain block is gated on `stepsLeft > 0`
alone. A motor node with no heartbeat and no beacon still pulses STEP. What stops
the car is electrical: the BMS holds the 12 V rail off. The emulator models the
firmware (buffer drains unconditionally) and tracks the rail separately, so a test
can tell "commanded but unpowered" from "not commanded". **Do not design a
challenge around "no heartbeat = no motion"** — it is false on the vehicle.

**2. C3 must key on same-`dir`, not on `rpm` alone.** The layering doc says
"detection keys on `rpm`" and "`rpm` already distinguishes the two cases", which
invites exactly the rule I first wrote. It passed the positive test and would have
failed in the only case that matters — see finding 3. The `dir` condition is what
actually discriminates; `rpm` is a filter on top of it.

**3. The resonance fix has undermined the spec's `rpm` discriminator.** The spec
rests on: pivot = `rpm` 50 (legitimate), straight = `rpm` 100 (never legitimate
while locked). But turn speed was raised to 100 on 2026-08-02 because 50 rpm sits
in the steppers' mid-band resonance and grinds. So the C1 pivot routine at `rpm`=50
**will grind audibly at every station**, and raising it to 100 makes `rpm` stop
discriminating. Detection survives either way *because* of the same-`dir` condition
(`test-detectors.py` case 2 proves a 100 rpm pivot stays silent), but the spec text
is now misleading and anyone porting it who simplifies to `rpm == 100 → C3` will
leak the C3 code on the car's own pivot. **Open decision before Phase 2: what rpm
does the pivot run at, and does the 102-step arc still hold there?**

**4. Two spec numbers are missing and are currently bench-tuned.** The C2 pair
window ("within a short window") is set to 250 ms — the dashboard keepalive is
150 ms, so both wheels are commanded inside one cycle. And "15 consecutive
qualifying commands" is ambiguous between CAN frames and command cycles (each cycle
sends two frames); this counts frames. Both need a decision before the RP2040 port.

**5. Emulated nodes must NOT use the wire-only filter** — the inverse of test
observers. A real node is a separate device, so everything reaching it is wire
traffic by definition; an emulator on the harness host that filters `MSG_DONTROUTE`
is deaf to exactly the frames the tests inject. On first run all four positive
detection cases failed while all three negative cases "passed", which is the worst
possible arrangement. Rule: **emulated nodes hear everything, test observers hear
only the wire.**

Consequence for the test runner: a DRIVE-side `Observer` cannot see the emulator's
own transmissions. "Did the emulated BMS emit a flag?" is answered by its counters;
"did the flag cross the gateway?" is answered by a DIAG `Observer`.

### The bug this phase caught

`ttos-dashboard.service` runs `DynamicUser=yes` — a transient unprivileged user —
while `ttos-provision.sh` staged `/etc/ttos/provision.src` as **600 root:root**. The
dashboard therefore could not read its own challenge identity on a provisioned car:
no VIN, no ECU serial, no unlock codes, every challenge dead.

It survived this long because it is invisible until the two halves meet. Every
hardware run so far was in factory mode, where the file legitimately does not exist
and the log line is the *same* "provision.src unreadable" error. A provisioned car
would have looked healthy — dashboard up, buses up, gateway live — and simply had
no challenges.

Fixed by staging the file `640 root:ttos-secrets`, adding that group to the image,
and giving the unit `SupplementaryGroups=ttos-secrets`. Verified on the DUT:
`CTF identity loaded for car 01`. The file stays unreadable to the panel and to
any contestant with a shell.

### Findings from Phases 4 and 5

**`crypto.subtle` is unavailable on the car's own panel.** SubtleCrypto exists only
in a secure context; the panel is plain HTTP on the AP address, so
`window.crypto.subtle` is `undefined` there. The C3 key transform ships in the
panel JS by design — contestants view source and run it — so building it on
SubtleCrypto would have thrown on the one platform it exists for, killing the
intended solution while nothing looked broken. It ships a bundled SHA-256, verified
in headless Chromium against a reference implementation over a genuinely non-secure
origin (`isSecureContext=false`, `crypto.subtle=undefined`), including the
multi-block padding path.

**ENOBUFS on the relay under sustained load.** The drive bus is 500 kbit and the
interface transmit queue is ten deep, so a client writing as fast as TCP allows
overran it: 80 frames issued in ~0 ms, 29 rejected. A contestant would have seen
the car stutter and concluded their forgery was wrong, then gone back to
re-deriving a CRC that was already correct — the exact unfair stalling the design
sets out to avoid. Bounded retry now paces the sender to bus speed.

Found only by driving the car through the relay. No unit test of `relaySend` would
have produced it.

**busybox `nc` silently loses commands.** It exits on stdin EOF, and closing a
socket with unread received data sends RST, discarding whatever the server had not
processed — 19 of 40 SEND commands. Not a relay bug, but contestants will reach for
`nc` first, so the banner tells them to read every response, and it ends with a
`READY` sentinel so clients need not count banner lines (adding a line silently
broke every client, which is how that was found).

**`Storage=persistent` was applied and did nothing.** `/var/log` is a symlink into
the `/var/volatile` tmpfs, so the "persistent" journal was still wiped on reboot.
systemd accepted the drop-in and `/var/log/journal` never appeared. It matters here
because there is no RTC and journald is the only record of a solve, and the
power-cycle that hands a station to the next team is exactly when it would vanish.
Fixed with `VOLATILE_LOG_DIR = "no"`; needs an image build, not a config push.

**A self-test check that Phase 5 made wrong in both directions.** It asserted
`TTOS_DASH_FRAMES` was empty, which was equivalent to checking behaviour until
per-session tier gating became the real control. After Phase 5 it fails a correctly
configured car, and an empty variable with broken tier logic would pass. It now
queries what an unauthenticated client actually receives from `/api/info`. General
lesson: assert the behaviour, not the setting that used to imply it.

### Fault injection

`drop <pct>`, `mute on|off`, `latency <ms>`, `lvc on|off`, `vbat <mv>`,
`sweep <lo> <hi> <secs>`, `rail on|off`, `reset`.

`mute` stops transmission but the controller still ACKs, so it is not bus-off.
Nothing in userspace can withhold an ACK — for a genuinely absent node (the ENOBUFS
case that has bitten this project repeatedly) take the interface down instead:
`sudo ip link set ttdrive down`.

## Not yet built

- **Test matrix runner** — brief §6. Covered so far: G5, C7, C8, partial C5.
  Outstanding: G1–G4, G6, all of D, C1–C4, C6, all of P.
- **Remote power control** — no `uhubctl`, no switchable hub. A wedged DUT still
  needs a human, which caps unattended iteration.
- **Serial console** — nothing on `/dev/serial/by-id`. When networking is what
  broke, there is currently no way in.
- **`rpiboot` reflash path** — not installed; flashing still means moving the card.
- ~~Static DUT address~~ — **done, better than the brief asks.** `192.168.4.133` is a
  static DHCP *reservation on the router*, keyed by MAC. Do NOT "fix" this by
  setting `TTOS_ETH_ADDRESS` in provisioning (bench brief §7): a DUT-side static
  address would fight the reservation, and it would have to be re-applied on every
  reflash, whereas the reservation survives them and follows the hardware.

## What the rig still cannot tell you

Unchanged from brief §8, and worth re-reading before trusting a green suite: node
firmware correctness, stepper/drivetrain behaviour (including the known left-motor
stutter), real bus timing, RF, physical containment. The emulator will test the
*specification*; one real car still has to close the loop before the fleet is
flashed.
