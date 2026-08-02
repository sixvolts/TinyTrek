# TTOS CTF — Challenge Layering: review + implementation plan

Response to `ttos-ctf-challenge-layering.md`, checked against the repo at `~/ttos-ctf`
on 2026-08-02. Sections below marked **BLOCKER** need an answer before the phase they
sit in can start; **CONFLICT** items are places the brief and the hardware/repo
disagree, raised rather than guessed per §0.

---

## A. Verified — the brief is right about these

| Claim | Status |
|---|---|
| `socketcand` bridges both buses (`-i can0,can1`) | ✅ confirmed — now **removed entirely** (§C) |
| `can0` is classic 500k, needs `FDMode=yes` + `DataBitRate=` | ✅ confirmed, `can0.network` — a 2-line edit |
| `can1` is already FD-configured | ✅ confirmed, `can1.network` |
| Drive frame is 6 bytes, unauthenticated | ✅ confirmed |
| Heartbeat `0x100` only transmits when the drive gate is set | ✅ confirmed, `main.go:544` |
| Dashboard SSE carries decoded frames for both buses | ✅ confirmed |

**Two §5.1 worries are already resolved in the baseline:**

- `CONFIG_CAN_GW=y` **is** already in `can_config.cfg`, and that fragment **is**
  attached to `linux-raspberrypi_%.bbappend` — the exact mistake the brief warns
  about was already found and fixed. `can-utils` and `can-utils-access` already ship
  in `ttos-ctf-image.bb`. Remaining work is one `cangw -L` check on the booted image.
- The Go `canbus` package **already does CAN FD** — `Open()` sets `CAN_RAW_FD_FRAMES`,
  and `Send()` writes a 72-byte `canfd_frame` when `Frame.FD` is set. The UDS-lite
  FD server needs no new socket plumbing.

**Two §5.6 "defects" do not exist.** `ttos-provision.conf.example` has exactly one
`TTOS_CONSOLE_PW_HASH` and no trailing whitespace, and the parser already trims:
`ttos-provision.sh:162` strips trailing comments, leading/trailing whitespace, and
surrounding quotes from every value. Nothing to fix.

---

## B. ~~BLOCKER~~ RESOLVED — `provisioning/` now populated and verified

*(Was: none of the §8 artifacts existed anywhere on arcana. Supplied 2026-08-02.)*

`fleet-table.csv`, `firmware-constants.h`, `OPERATOR-SECRETS.md` and `gen-fleet.py`
were handed over and now live in `provisioning/`. The two derived artifacts that were
**not** supplied — the 8 `ttos-provision-carNN.conf` files and `judge-packet.md` —
were re-rendered **deterministically from the CSV**, not regenerated. No value
changed. See `provisioning/README.md`.

**Cross-checks run against the supplied data — all pass:**

- 8 VINs match `gen-fleet.py`'s ISO 3779 algorithm exactly, and each check digit
  validates. VIN generation is deterministic from car number, so the VINs were
  independently recomputed before the CSV arrived and matched it.
- Data IDs and C2/C3 codes in `firmware-constants.h` match the CSV for all 8 cars.
  All 16 Data IDs in range, L≠R per car, no fleet-wide collisions.
- 24 unlock codes: correct length, challenge prefix, and restricted alphabet.
- All 8 `console_hash` values verify against their plaintext in `OPERATOR-SECRETS.md`.
- VIN / ECU serial / PSK / codes / channels unique per car.

**`gen-fleet.py` must never be re-run** — beyond §8's warning, it imports `crypt`
(removed in Python 3.13) and cannot run on arcana anyway. `render-fleet.py` is the
safe deterministic path for re-emitting derived files.

**Secrets policy conflict, resolved toward the baseline.** `gen-fleet.py`'s docstring
says "commit the output," but `.gitignore` already carries an explicit "never commit
provisioning data" rule. Per §0 the existing behaviour wins: `provisioning/` is
gitignored except the two scripts and its README, and the data files are mode 600.
**`provisioning/` must be backed up off this repo** — losing `fleet-table.csv` means
reflashing every node in the fleet.

---

## C. RESOLVED — `socketcand` dropped entirely. Tap or nothing.

CAN requires a **second physical node to ACK**. `socketcand` is a TCP front-end to
the Pi's *own* `can0` controller — it does not add a node to the bus. So with no tap
device physically connected, a contestant's transmit gets no ACK, retries, and backs
up into `ENOBUFS`; the UDS server's responses and the gateway's forwarded flag frames
fail the same way. Restricting it to the DIAG bus would not have fixed that.

**Decision (2026-08-02): removed from the image.** Diagnostic access is the physical
side tap only. `socketcand` is out of `IMAGE_INSTALL` in `ttos-ctf-image.bb`, with the
reasoning recorded in-file; the recipe stays in the layer unbuilt. Port 29536 no
longer exists on a car. The C3 relay is a **separate authenticated service** and does
not reuse it.

**Revised Phase 0 acceptance:** a contestant with no physical tap has *no* path to
either CAN bus — the dashboard base tier over WiFi is the entire remotely reachable
surface. A tapped contestant reaches the DIAG bus only.

**Placard must say "CAN FD adapter required, physically connected."** The judge
packet's "no traffic at all" troubleshooting row now covers the common case.

### C.2 — the dashboard is not reachable over Ethernet anyway

`TTOS_DASH_ADDR=192.168.244.1:80` binds the AP address only. Phase 0's acceptance
mentions a contestant "on Ethernet" reaching the dashboard; today that requires WiFi.
Recommend leaving it AP-only (it is the safer posture) and correcting the criterion.

---

## D. PROPOSAL — BMS stand-down via the heartbeat's `flags` byte

§5.3 says C2 detection "stands down once the C2 code is redeemed on this car," and
§6 says detection rules stand down after C3. Redemption happens in the **dashboard**;
detection lives in **BMS firmware**. Nothing connects them, and the BMS cannot see
HTTP. Without a channel, the first forward command after a C3 unlock re-triggers C2 —
same `dir` on both motors *is* the C2 signature — and the car leaks `0x7D1` for the
rest of the session.

### The mechanism: an arm mask, not a redemption flag

`0x100` is already specified as `[seq][flags]` with `flags` unused. This **defines a
reserved field**; it does not change the frame layout, the ID, the rate, or `seq`.

```
0x100  Pi -> nodes, 5 Hz, classic, DLC 2
  byte 0  seq    free-running counter (unchanged)
  byte 1  flags  bit0 (0x01)  C2 detector ARMED
                 bit1 (0x02)  C3 detector ARMED
                 bits 2-7     reserved, transmit 0
```

A locked car transmits `flags = 0x03`. After C2 is redeemed → `0x02`. After C3 → `0x00`.

**Positive arming, not "redeemed" bits — this is the important detail.** With
redeemed-semantics, `flags = 0` means *armed*, so any failure (Pi bug, short frame,
old firmware) leaves detection live and the car leaks the C2 code during normal
driving. With arm-semantics, the same failures leave detection **disarmed**: the
challenge doesn't fire, someone reports "this station is broken," and it gets fixed.

A dead station is recoverable. A silently leaked flag is not. So the fail-safe
direction is *disarmed*, and the BMS treats any of these as `armMask = 0`:
heartbeat older than 1 s (same timeout the motor nodes already use), or DLC < 2.

### Why mirrored state rather than a one-shot event

The BMS **mirrors** the Pi's current view at 5 Hz rather than latching a "C2 redeemed"
event. That buys three things for free:

- **Loss-tolerant.** A dropped frame self-corrects in 200 ms. A one-shot latch that
  misses its frame stays wrong forever.
- **BMS power-cycle safe.** A node that resets mid-event re-syncs on the next
  heartbeat, with no re-announcement protocol.
- **Reset falls out of §7.3 for nothing.** Restarting the dashboard clears the
  car-level record → `flags` returns to `0x03` → both detectors re-arm. No separate
  reset path to write or remember.

### Source of truth: the car-level record, not the session tier

`flags` must be driven by the **per-car "tiers unlocked since last reset" record**
(§7.2), *not* the caller's session tier. If it were session-scoped, opening a second
browser tab — or a session idling out after 10 min — would silently re-arm detection
mid-event and start leaking codes again.

### Emission policy (fairness)

While armed and the trigger condition holds, the BMS emits its code frame at **2 Hz**,
stopping when the condition clears or the detector disarms. Repeatable emission means
a contestant who solved it without a sniffer running can just do it again. A
single-shot emission would make the challenge a coin flip on whether they were
capturing at that instant.

### Cost

~15 lines in `TinytrekBMS.ino`, one field in the Pi's heartbeat builder. No new CAN
ID, no new transmitter, no change to `0x116`.

### Fallback if you'd rather not touch `0x100`

A dedicated `0x101` "CTF state" frame at 2 Hz carrying the same mask. Functionally
identical; costs one more ID and one more periodic transmitter. The heartbeat
approach is better only because the field already exists and is already being sent.

---

## E. Spec gaps to close before firmware is flashed

Firmware touches all 8 cars ×3 nodes, so these must be exact before the first flash.

1. **CRC-8 Data ID mixing — proposal in §I below.** §4.1 gives the CRC parameters but
   not how the Data ID enters the computation. The Pi and the motor firmware must
   agree bit-for-bit, and the choice interacts with solvability. Treated in full in §J.
2. **`nonce` is outside the CRC.** Bytes 0–5 are covered; `nonce` is byte 6. This has
   a bigger consequence than it looks — see §I.2. Recommend changing it.
3. **CAN FD lengths are quantised** to 0–8, 12, 16, 20, 24, 32, 48, 64. The kernel
   rejects anything else. UDS responses must be padded up to the next valid length.
   A `0x31` positive response with an 8-byte flag is `71 01 <rid_hi> <rid_lo>` + 8 =
   12 bytes — already valid. Longer DIDs (VIN = 17 chars → 20-byte payload) need pad
   bytes, and the pad value should be fixed (`0x00`) so responses are reproducible.
4. **The Data ID lives on the Pi.** The CTF service must compute valid CRCs to run
   the pivot routine, so a root shell is a complete C3 bypass. Consistent with §5.4's
   "no filesystem access" on the relay; worth stating explicitly in the judge packet.
   `root` is locked and the `ttos` password is per-car random, so this is acceptable —
   but SSH password auth (§3 Phase 0 item 4, "leave it") is the exposed edge.

---

## F. Architecture recommendation — one binary, not two

§3 Phase 1 offers "a Go service alongside the dashboard, or extend it."

**Recommend extending the existing dashboard process**, adding new files under
`cmd/dashboard/` rather than a second service. Reasons:

- Unlock tier state is read by the UI layer and written by code-redemption, and
  redemption state must reach the heartbeat emitter (§D). One process = a mutex; two
  processes = inventing an IPC channel for no gain.
- §7.3 wants reset to clear unlock state by restarting services. One unit is simpler
  and cannot half-restart.
- Reuses `internal/canbus` with no duplication and no change to the Yocto recipe
  beyond new source files.

Trade-off: a dashboard crash takes the UDS server with it. Mitigated by
`Restart=always` and the fact that a crash *should* reset challenge state anyway.

New files, all additive:

```
cmd/dashboard/uds.go        UDS-lite server (0x7DF/0x7E0/0x7E8), sessions, S3 timer
cmd/dashboard/routines.go   pivot, decoy, self-test/bridge window
cmd/dashboard/protect.go    CRC-8 J1850 + Data ID, 8-byte drive frame builder
cmd/dashboard/tiers.go      session cookies, tier gating, code redemption
cmd/dashboard/identity.go   VIN / serial / salt / codes loaded from /etc/ttos
cmd/dashboard/gateway.go    cangw policy apply/revoke for the bridge window
```

`heartbeatLoop()` (`main.go:544`) moves out from under the drive gate and gains the
flags byte. **The dashboard must stop emitting `0x100` from its old path** or two
heartbeats race.

---

## G. Phase plan

Phase order follows the brief. Phase 0.5 is new (housekeeping, not scope creep).

### Phase 0.5 — commit the baseline first
16 files are modified and uncommitted, including all three firmware sketches, the
dashboard, and the AP/provisioning fixes from the last two sessions. Layering
challenges on an uncommitted tree means a bad challenge change cannot be bisected
away from a good baseline fix. Commit as-is, tag `baseline-v1`, then start.

### Phase 0 — close the bypasses
- **`socketcand` removed from the image** (done — §C).
- `ttos-selftest`: fail loudly if `/etc/ttos/factory` exists; add a pre-event check
  block covering variant marker, drive gate, factory marker, `cangw -L`, both bus
  states.
- Dev image visual distinction: already has hostname + `/etc/issue` banner; add the
  variant marker to the dashboard header so it is unmistakable on screen.
- Gate `/events` frame payloads by tier (the tier plumbing lands in Phase 5, so
  Phase 0 ships the blunt version: **no raw frames without a tier**, default deny).
- **Acceptance (revised per §C):** an untapped contestant on WiFi sees the dashboard
  base tier and no DRIVE-bus frames by any route; a tapped contestant sees the DIAG
  bus only.

### Phase 1 — gateway + CTF service skeleton
- `can0.network`: `FDMode=yes`, `DataBitRate=1000000`. Fix the misleading comments in
  both `.network` files — they still describe the pre-2026-08-01 (inverted) roles.
- `ttos-cangw.service`: default policy DRIVE→DIAG for `0x7D1`/`0x7D2` only, nothing
  inbound, no `-X`. Ordered `After=network-online.target`; **not** `Before=` anything
  in `multi-user.target` (SYSTEM-OVERVIEW §9 ordering cycle).
- Heartbeat moves, with the flags byte from §D.
- UDS-lite server: `0x10`, `0x3E`, `0x22`, `0x31`; single-frame FD only; S3 = 5000 ms.
- **Bench-testable on `vcan0`** end to end before any car is involved — the same rig
  used to validate the heartbeat/beacon work.

### Phase 2 — C1 (discovery)
Pivot routine, 102 steps, `rpm`=50, opposite `dir`, one command per wheel. Flag in the
`0x31` positive response. Per-car unlock codes are in hand (§B).

Also lands here: extend `ttos-provision.sh` to parse and persist the seven new keys
(`TTOS_VIN`, `TTOS_ECU_SERIAL`, `TTOS_FLEET_SALT`, `TTOS_DATAID_L/_R`,
`TTOS_CODE_C1/_C2/_C3`) into `/etc/ttos/provision.src`, fail loud when any is
missing, and shred the FAT copy as it already does. The existing parser already
trims and handles CRLF, so this is additive key handling only.

### Phase 3 — C2 (bridge window + composition)
8-byte protected drive frame + CRC rejection in motor firmware; snapshot DID; decoy
routine (`0x33` always); self-test routine (extended session only, `0x7F 31 7E`
otherwise) opening the inbound bridge 5 s; motors-idle precondition; BMS same-`dir`
detection → `0x7D1`; stand-down via §D.

Firmware freeze point — after this, changes cost 16 motor flashes.

### Phase 4 — C3 (wireless takeover)
Authenticated relay on the DRIVE bus, WiFi only, no shell/FS/config access.
`0xF190` VIN (default session), `0xF18C` serial (extended only), `deriveServiceKey()`
in the base-tier JS, constant-time compare, malformed-vs-wrong distinction,
rate limiting, monotonic sequence logging. BMS sustained-forgery detection (15
qualifying commands in 3 s, `rpm`=100) → `0x7D2`.

### Phase 5 — tiers, ops, judging
Tier ladder per §6; `crypto/rand` cookie sessions, 10-min sliding idle, `/events` and
`/api/control` both gated, re-checked per event. `Storage=persistent` for journald.
Per-car server-side tier record for judges. Reset command runnable from the serial
console. Per-car placard generation.

---

## H. Things I will not do without being told to

- Re-run or invent fleet values if the real `provisioning/` output exists elsewhere (§B).
- Change the `0x100` flags byte (§D) — it is a protocol change the brief calls unchanged.
- Touch the `can0`/`can1` naming. The brief's role-name convention resolves this
  cleanly; renaming would invalidate the placards and every note from the last week.
- Implement the §5.4 heartbeat-spoofing extra layer. The brief calls it first to cut,
  and it is the only item that makes C3 depend on firmware timing behaviour.

## I. PROPOSAL — CRC-8 Data ID mixing, and a correction

### I.1 The exact scheme

§4.1 fixes the CRC parameters but not how the untransmitted 16-bit Data ID enters the
computation. Proposed, modelled on AUTOSAR E2E Profile 1 (which protects a frame with
CRC-8/SAE-J1850 keyed by exactly such a Data ID):

```
crc8 = CRC8_SAE_J1850( dataid_lo || dataid_hi || <covered bytes> )
       poly 0x1D, init 0xFF, xorout 0xFF, no reflection
```

Data ID **prefixed, low byte first, then high byte**, then the covered payload bytes.
Prefixing is the AUTOSAR convention, and it is the ordering a contestant who knows E2E
will try first — which is a feature, not a leak, in a car-hacking CTF.

Concretely, for car 01's left motor (`TTOS_DATAID_L = 0x60B0`), a forward pivot:

```
covered:  00 00 00 66 01 32          steps=102, dir=fwd, rpm=50
fed:      B0 60 | 00 00 00 66 01 32
```

Motor firmware recomputes over the received bytes with its compiled-in
`TTOS_DATAID_L`/`_R` and drops the frame silently on mismatch. No NRC, no error frame
— an oracle here would shortcut C2.

### I.2 Correction: extra captures do **not** narrow the Data ID

My earlier note said one captured frame leaves ~256 candidate Data IDs and a second
narrows it to ~1. **That is wrong**, and the error matters for how C3 is framed.

A CRC is linear over GF(2), so a fixed-length prefix contributes a **constant 8-bit
XOR offset to every frame, independent of the payload**. Verified by exhaustive search
over all 65,536 Data IDs, for both prefix and suffix orderings:

| Captures | Candidate Data IDs remaining |
|---|---|
| 1 | 256 |
| 2 | 256 |
| 3 | 256 |

Additional captures give **zero** additional information. This is structural, not a
parameter-tuning issue, and it holds whichever end the Data ID is fed in at.

**What this means in practice — and it is fine:**

- Contestants recover the 8-bit *offset*, i.e. a 256-member equivalence class. Every
  member produces byte-identical CRCs, so **forgery works perfectly** without ever
  identifying the true Data ID. C3 is fully achievable.
- Recovering the offset, once the algorithm is known, is a single XOR — no brute
  force. The real work is *identifying the algorithm*, not extracting the key.
- **Judges must not expect a specific Data ID value.** "Recover the Data ID" is only
  true up to the 256-way class; `judge-packet.md` lists exact values, which will not
  match what a correct solve produces. Worth a line in the packet.

### I.3 Recommended deviation: cover the `nonce` (bytes 0–6, not 0–5)

This is the one place I'd change the spec, and it is a solvability issue.

A pivot always sends `steps=102`, `rpm=50`, and `dir ∈ {fwd, rev}`. With the CRC
covering only bytes 0–5, that is **exactly two distinct CRC inputs that can ever be
captured, per motor** — for the whole event. The `nonce` is the only byte that varies
per transmission, and it sits outside the covered range.

Two samples is not enough to identify the CRC model. Sweeping all 256 polynomials ×
4 reflection combinations (init/xorout/Data ID all collapse into the one unknown
offset, so they are not free parameters):

| CRC coverage | Distinct inputs capturable | Candidate CRC models surviving |
|---|---|---|
| bytes 0–5 (as specified) | **2** — permanently | **4**, and no further work resolves it |
| bytes 0–6, 2 captures | unlimited | 5 |
| bytes 0–6, **3 captures** | unlimited | **1** |

As specified, the challenge is still *solvable* — 4 surviving models means trying 4
forgeries — but the identification step degenerates into guessing rather than
analysis, and a team pointed at CRC RevEng gets an underdetermined answer no matter
how long they grind. Covering the nonce makes every transmission a fresh
(input, output) pair, and three captures pin the model uniquely. It is the difference
between "guess it's J1850" and a clean, tool-supported reverse-engineering exercise.

**Nothing else changes.** The nonce stays unvalidated (the motor never inspects its
value, it just has to be consistent with the CRC), replay still works verbatim, and
composed C2 frames are captured frames so they remain valid. Cost is one constant in
two places.

Revised layout — same 8 bytes, one more byte covered:

```
 byte:  0    1    2    3     4     5      6       7
      +----+----+----+----+-----+-----+-------+-------+
      |   steps (u32 BE)  | dir | rpm | nonce | crc8  |
      +----+----+----+----+-----+-----+-------+-------+
      |<-------------- covered by crc8 ------>|
```

If you'd rather hold the spec exactly as written, say so and I'll implement bytes 0–5
— the challenge still works, with the 4-way model ambiguity noted in the judge packet
so nobody calls a correct-but-different forgery a failure.

---

## J. Known risk carried in from the baseline

The **left motor stutter** (SYSTEM-OVERVIEW §10) is still unexplained and suspected to
be node/wiring. Phase 3 adds *silent* CRC rejection to motor firmware — a frame-level
fault will then present as identical silence. Recommend resolving the stutter, or at
minimum confirming it is not frame corruption, **before** the Phase 3 firmware freeze.
The status LEDs added last session should localise it on the next bench run.
