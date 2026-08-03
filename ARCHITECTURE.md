# TTOS CTF — architecture and challenges, as built

State of the system on 2026-08-03, written from the code rather than the design
docs. Where this disagrees with `ttos-ctf-challenge-layering.md`, **this file is
what is actually implemented** — the deviations are listed in section 8 with the
reason for each, and those are the ones worth arguing with.

---

## 1. Physical layout

One Raspberry Pi 4 and three microcontroller nodes per car, on two CAN buses.

```
                    ┌──────────────────────────────────────┐
                    │  Raspberry Pi 4  (ttos-dashboard)    │
                    │  panel · UDS server · gateway · relay│
                    └───┬──────────────────────────┬───────┘
              can1      │                          │   can0
          DRIVE bus     │                          │   DIAG bus
     classic 500 kbit   │                          │   CAN FD 500k/1M
                        │                          │
        ┌───────┬───────┴───────┐                  │
        │       │               │                  │
   ┌────┴───┐ ┌─┴──────┐ ┌──────┴──┐        ┌──────┴─────────┐
   │ L motor│ │ R motor│ │   BMS   │        │ contestant tap │
   │ SAMD21 │ │ SAMD21 │ │ RP2040  │        │  (FD adapter)  │
   │+MCP2515│ │+MCP2515│ │+MCP2518 │        └────────────────┘
   └────────┘ └────────┘ └─────────┘
```

**Bus roles are names, not numbers.** The drive bus is the *higher*-numbered
interface (`can1`), which is the opposite of the original design doc. Verified on
hardware 2026-08-01 and again on every flash since.

| | DRIVE (`can1`) | DIAG (`can0`) |
|---|---|---|
| Speed | classic CAN 2.0, 500 kbit | **CAN FD**, 500 kbit arb / 1 Mbit data |
| Who is on it | 2 motors, BMS, Pi | Pi, and a contestant's adapter |
| Idle traffic | heartbeat + beacon, ~23 frames/s | **silent** until a request arrives |

`can1` is classic because the motor nodes are MCP2515, which has no FD support at
all — an FD frame there is a form error that drives them error-passive and then
bus-off, stopping propulsion. FD is disabled in configuration so the Pi cannot emit
one even by mistake.

`can0` is FD because responses do not fit otherwise: VIN 20 bytes, pivot response
12, snapshot 23. **A classic-only adapter can transmit every request and will never
receive an answer.**

---

## 2. CAN message map

| ID | Direction | Bus | Payload |
|---|---|---|---|
| `0x100` | Pi → nodes, 5 Hz | DRIVE | `[seq][armMask]` — liveness + detector arming |
| `0x111` | Pi → left motor | DRIVE | drive command |
| `0x113` | Pi → right motor | DRIVE | drive command |
| `0x115` | Pi → BMS | DRIVE | `[0x01]` 12 V on / `[0x02]` off |
| `0x116` | BMS → all, 5 Hz | DRIVE | `[v_hi][v_lo][pwr][flags]` pack mV, rail state, LVC |
| `0x7D1` | BMS → bus, 2 Hz while triggered | DRIVE→DIAG | C2 code, 8 ASCII |
| `0x7D2` | BMS → bus, 2 Hz while triggered | DRIVE→DIAG | C3 code, 8 ASCII |
| `0x7DF` / `0x7E0` | tester → Pi | DIAG | UDS functional / physical request |
| `0x7E8` | Pi → tester | DIAG | UDS response |

### Drive command format

```
 byte:  0    1    2    3     4     5      6       7
      +----+----+----+----+-----+-----+-------+-------+
      |   steps (u32 BE)  | dir | rpm | nonce | crc8  |
      +----+----+----+----+-----+-----+-------+-------+
      |<-------------- covered by crc8 ------>|
```

`dir` `0x01` forward / `0x02` reverse / `0x00` stop-and-clear. `steps` are **added**
to a drain buffer capped at 250; a direction change clears it.

`crc8` is CRC-8/SAE-J1850 (poly `0x1D`, init `0xFF`, xorout `0xFF`) over bytes 0–6
**keyed by a 16-bit Data ID that is never transmitted**, prefixed low byte first —
AUTOSAR E2E Profile 1 convention. Agreement verified across three implementations
(Go, Python, C++) against bytes captured off the wire.

A **6-byte legacy form** (bytes 0–5, no nonce or CRC) also exists and is what the
Pi's own control pad sends. See section 4 for why both exist.

---

## 3. Where every secret lives

| Value | Per car or fleet-wide? | Lives on | Contestant-visible? |
|---|---|---|---|
| WiFi SSID / PSK | **per car** | Pi (provisioned) | on the placard |
| Hostname | **per car** | Pi (provisioned) | yes |
| Console password | **per car** | Pi (provisioned) | no — operator only |
| VIN | **per car** | Pi (provisioned) | via DID `0xF190`, by design |
| ECU serial | **per car** | Pi (provisioned) | via DID `0xF18C`, extended session |
| Fleet salt | fleet-wide | Pi (provisioned) → **served in the panel JS** | yes, deliberately |
| **Data IDs L/R** | **fleet-wide** | Pi (provisioned) | **no — this is Challenge 3** |
| **Codes C1/C2/C3** | **fleet-wide** | Pi + BMS firmware | emitted by the car on solve |

**Changed 2026-08-03: Data IDs and codes are fleet-wide, not per car.** Reason: a
car dying mid-challenge used to cost that team their work, because the values they
recovered did not apply to a replacement. Uniform values make any car a drop-in
spare. Cost: a code that is overheard now works on any car — covered by the judging
path (section 7), which is car-level and server-side.

L and R Data IDs still differ **from each other**. They are separate binaries
either way, since the CAN id differs.

---

## 4. Where message protection is enforced — and why it moved

**The Pi enforces, at its two inbound gates. The motor nodes do not.**

That is a deviation from the layering doc, made 2026-08-03, and it is the one most
worth reviewing.

The doc puts the CRC check in motor firmware. That cannot coexist with two hard
requirements:

1. an **unprovisioned** car must drive, and
2. the car must drive **after Challenge 3**.

An unprovisioned Pi has no Data IDs — they arrive in `provision.src` — so it cannot
construct a protected frame at all. Motor nodes that accept nothing else leave the
car undrivable until it is provisioned, and leave the post-C3 reward dead. Putting
the Data IDs in the image instead would mean fleet secrets in a build artifact.

Moving the check to the Pi works because **contestants have exactly two routes to
the drive bus and the Pi owns both**:

```
  C2 bridge window  DIAG → DRIVE, 0x111/0x113 only, 5 s, opened by the self-test
  C3 relay          authenticated TCP, WiFi only, raw CAN onto DRIVE
```

There is no third path — guaranteeing that is what the gateway policy exists for.
So "recover the Data ID or the car will not move" still holds, which is all C3
rests on.

**Given up:** protection against someone with *physical* drive-bus access. Already
outside the threat model — that person can drive the car directly and skip every
challenge.

**Gained:** motor nodes stay baseline. 8 flashes instead of 24, and the
silent-rejection debugging hazard is gone from the hardware hardest to instrument.

Both gates **drop silently and answer uniformly**. The relay says `OK queued`
whether or not it forwarded, because answering "bad CRC" would be a free oracle: a
contestant could sweep the protection byte and read the reply instead of recovering
the Data ID. The only feedback is whether the car moved.

---

## 5. The gateway

In-kernel `cangw`, installed at boot, two rules, nothing inbound:

```
cangw -A -s can1 -d can0 -f 7D1:7FF     flag frames out
cangw -A -s can1 -d can0 -f 7D2:7FF
```

**Nothing else crosses in either direction.** Drive traffic must not go outbound or
contestants could build the C2 corpus by passive sniffing, which would collapse C2
from "interact with the car, then compose" into "watch and replay". The sanctioned
corpus source is the snapshot DID instead.

The C2 inbound bridge is *not* a cangw rule — it is Go, because installing kernel
gateway rules needs `CAP_NET_ADMIN` and `ttos-dashboard` is the one process
contestants can reach over the network.

---

## 6. The three challenges

### C1 — discovery

**Skill:** find the diagnostic server, enumerate DIDs, invoke a routine.

1. Tap the DIAG bus. It is silent until you send something.
2. `0x22 F1 90` → VIN.
3. Sweep `0xF1xx`, find `0xF1A0` (snapshot) and `0xF1A1` (telematics endpoint).
4. `0x31 01 02 01 <dir>` → RoutineControl, pivot. **The car turns 45° in place**,
   and the positive response carries the C1 code in its routineStatusRecord.

Routine IDs sit in a contiguous plausible block (`0x0201`–`0x0203`) so finding one
makes the others the obvious next probe. The puzzle is the window and the
composition, not the search.

### C2 — bridge window and composition

**Skill:** session control, reading an NRC and acting on it, composing valid frames.

1. `0x22 F1 8C` (ECU serial) in the default session → **NRC `0x7F`**. The NRC names
   its own solution.
2. `0x10 03` extended session, retry → serial. This gate is what makes C3 require
   the C2 work rather than merely follow it.
3. Routine `0x0202`, obviously named "enable bridging" → **always `0x33`
   securityAccessDenied, in every session**. It is a decoy. That door never opens.
4. Routine `0x0203`, "self-test" → **`0x7F 31 7E` in the default session**, runs in
   extended. While it runs it opens the inbound bridge for **5 seconds**.
5. Invoke the pivot, read `0xF1A0` — the snapshot returns the last command sent to
   each motor **with protection bytes intact**. Do it in both directions to get a
   forward frame and a reverse frame.
6. **Compose**: send both motors the frame that drives them the *same* way. That is
   a translation, which no legitimate interface commands while the panel is locked
   — the pivot always counter-rotates.
7. Replay inside the 5 s window. The BMS sees it and emits **`0x7D1`**, which the
   gateway forwards to DIAG.

The panel's bridging indicator flickers to enabled during the window. That is the
fairness tell and it lives in the **base** tier.

### C3 — wireless takeover

**Skill:** assembling everything, plus recognising auth logic shipped to a client.

1. The panel's JavaScript contains `deriveServiceKey(vin, serial)`, served at base
   tier. Contestants view source and find it.

   ```
   key = SHA-256(vin + ":" + serial + ":" + FLEET_SALT), hex, first 16 chars
   ```

   It uses a **bundled SHA-256, not `crypto.subtle`** — SubtleCrypto only exists in
   a secure context and the panel is plain HTTP on the car's AP, so the obvious
   implementation would be `undefined` and throw on the one platform it is for.

2. VIN comes from C1, serial requires the C2 session work. Neither is guessable.
3. DID `0xF1A1` gives the telematics endpoint (`192.168.244.1:29537`). The C2 panel
   tier also names it outright — two discovery paths, because a door findable only
   one way fails entirely for a team that misses that way.
4. Join the car's WiFi, `AUTH <key>` on the relay. Constant-time compare;
   malformed keys are told so, well-formed wrong ones get a generic failure;
   per-IP rate limiting.
5. Drive the car with forged protected frames. **This is where the Data ID work
   pays off** — the relay drops anything with a bad CRC.
6. Sustained same-direction commanding at a non-pivot rpm → BMS emits **`0x7D2`**.

**The tier separation is mechanical, not policy.** C2's composed replay uses
captured frames, which structurally carry the pivot rpm (75). C3 fires only on a
rpm that is *not* the pivot's. So a C2 solve can never accidentally yield the C3
code. `bench/test-detectors.py` asserts both directions of that.

---

## 7. Detection, arming, and judging

Detection lives in **BMS firmware**, watching the drive bus it is already on.

| | condition |
|---|---|
| **C2** | both motors commanded the **same direction** within 250 ms |
| **C3** | 15 consecutive same-direction commands within 3 s at rpm ≠ `PIVOT_RPM` |

Both are gated by the **arm mask** in heartbeat byte 1 — bit 0 = C2 armed, bit 1 =
C3 armed. **Positive arming**: any failure (stale heartbeat, short frame, no
heartbeat) yields mask 0 and leaves detection **disarmed**. A dead station is
recoverable; a silently leaked flag is not.

The mask is driven by the **car-level** record of tiers redeemed since last reset,
*not* by the browser session's tier. If it were session-scoped, opening a second tab
would silently re-arm detection mid-event and the car would leak codes during
ordinary driving.

**Judging:** `/api/judge` returns that car-level record plus a monotonic sequence
number. Judges cannot read progress off the panel — unlock state is session-scoped,
so they would see their own session. Timestamps are untrustworthy (no RTC, no
internet), so verification is by sequence.

**Reset:** `sudo ttos-reset` on the car clears every unlock, re-arms the detectors,
and reloads the gateway. Runnable from the serial console with no laptop, because
between rounds that is all an operator has.

---

## 8. Panel tiers

One embedded bundle, no tier-dependent assets. Gating is server-side, re-checked
**per SSE event** — a stream opened at tier 3 stops carrying tier-3 payloads the
moment the session lapses.

| Tier | Unlocked by | Adds |
|---|---|---|
| Base | — | indicators, bridging tell, docs, flag entry, `deriveServiceKey()` |
| 1 | C1 code | DIAG-bus frame tab |
| 2 | C2 code | DRIVE-bus frame tab, telematics named outright |
| 3 | C3 code | full drive controls; detectors stand down |

Sessions are `crypto/rand` tokens in an `HttpOnly` cookie, server-side tier map,
10-minute **sliding** idle. In memory, so a service restart clears every unlock.

`stop` on `/api/control` is **ungated on purpose** — an e-stop behind an unlock is
not an e-stop.

---

## 9. Deviations from the layering doc

| # | Doc says | Built as | Why |
|---|---|---|---|
| 1 | CRC covers bytes 0–5 | **0–6, includes the nonce** | 0–5 gives exactly 2 capturable inputs *ever*, leaving 4 indistinguishable CRC models that no further capture resolves. Covering the nonce makes 3 captures pin it uniquely |
| 2 | Motor firmware validates the CRC | **the Pi's inbound gates do** | node-side is incompatible with "unprovisioned must drive" and "must drive after C3" — section 4 |
| 3 | Bridge window via inbound `cangw` rules | **Go forwarder** | kernel rules need `CAP_NET_ADMIN` on the one network-reachable process |
| 4 | Per-car Data IDs and codes | **fleet-wide** | a dying car must not cost a team their work |
| 5 | pivot `rpm`=50 | **75** | 50 = 167 pps, inside the steppers' resonance band: it grinds and drops steps. 100 is clean but is the speed a contestant would drive at, which would let them evade C3 by accident |
| 6 | `socketcand` on the DIAG bus | **removed entirely** | unauthenticated TCP path to a CAN bus; the C3 relay is a separate authenticated service |
| 7 | emulator refuses motion without heartbeat | **drains regardless** | that is what the real firmware does; the interlock is electrical (the BMS holds the rail off), not in code |

---

## 10. What is flashed where

| Target | Count | Unique? | Build |
|---|---|---|---|
| Pi image (`.wic`) | 8 | **identical** | `./build.sh` |
| `ttos-provision.conf` | 8 | **per car** | `provisioning/ttos-provision-carNN.conf` |
| BMS firmware | 8 boards | **identical binary** | `TTOS_CHALLENGE=1 ./build.sh <fqbn> TinytrekBMS` |
| Motor firmware | 16 boards | **identical, baseline** | already flashed — no reflash needed |

**The BMS is the only node needing a new flash**, and all 8 get the same binary.
Per-car difference is one file on the Pi's boot partition.

---

## 11. Known gaps

- **No node firmware has run yet.** The BMS challenge build is compile-verified and
  its rules are validated against a Python model, but no RP2040 has executed them.
- **Pivot at 75 rpm is unconfirmed mechanically.** 250 pps should clear the
  resonance band; only a real drivetrain can say.
- **Motor stutter unexplained.** Bus contention is ruled out with data (0.34% load,
  zero bus errors); the Pi's command stream is metronomic (stdev 1.6 ms). Prime
  suspect is the browser-driven keepalive over WiFi, unmeasured.
- **Placards and judge packet not written.** The judge packet still describes
  per-car values and implies a specific Data ID, when a correct solve recovers a
  256-member equivalence class.
- **Two soaks outstanding:** C5 (30 min of legitimate operation → zero flags) and
  D11 (ENOBUFS tolerance with the DIAG bus down).
