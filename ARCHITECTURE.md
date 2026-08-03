# TTOS CTF — architecture and challenges, as built

**This document is the source of truth for the CTF design.** It describes the system
as built and as intended; where an older design note disagrees with it, this file
wins. Written from the code rather than from the design docs, and kept current as
the implementation changes.

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

## 4. Node provisioning, and where protection is enforced

**Nothing per-car is compiled into firmware.** There is one motor binary and one BMS
binary for the whole fleet. The Pi is the only thing that carries per-car data.

```
SETUP (automatic, first boot with a provisioning file present)
    → Pi bursts Data IDs, unlock codes and detector thresholds on 0x101 for ~6 s
    → nodes latch them and WRITE THEM TO FLASH
    → Pi records that it is done, and never repeats it

RUNTIME (competition)
    → nodes read from flash at boot; NOTHING is transmitted
    → a provisioned node IGNORES further config frames
    → the relay refuses to transmit 0x101 or 0x100 at all
```

Provisioning happens **automatically on the first boot after the car is
provisioned** — drop the config on the boot partition, power on, done. Nobody shells
into eight cars. `ttos-provision-nodes` exists only for the case where a node is
replaced afterwards.

**Why it is one-shot rather than periodic.** An earlier version
broadcast the set at 1 Hz forever and filtered it out of the relay's read path. That
put the value Challenge 3 exists to recover permanently on the bus a contestant is
attacking, with a single Pi-side filter between them and it. It happens once, at
setup, with nobody listening — and not on every restart, which during an event is
whenever a team asks for a power cycle.

**Why a provisioned node refuses reconfiguration.** Otherwise anyone reaching the
drive bus could hand the motors a Data ID they already knew and forge freely,
collapsing Challenge 3 into "get relay access". Re-provisioning means erasing or
reflashing the node — physical access, deliberately.

**Unconfigured is permissive.** A node with nothing stored accepts the unprotected
6-byte command, because an unprovisioned car must still drive — a hard requirement,
and a car that will not move until provisioned is useless during setup.

That is fail-open, so **the Pi's inbound gates validate protection independently**.
Contestants have exactly two routes to the drive bus and the Pi owns both: the 5 s
C2 bridge window and the C3 relay. Both drop unprotected or bad-CRC frames silently.
The two layers cover each other — a node that missed its config still cannot be
driven by a contestant, and a mistake in a gate still leaves the node enforcing.
Neither is load-bearing alone.

Both gates **answer uniformly**. The relay says `OK queued` whether or not it
forwarded, because answering "bad CRC" would be a free oracle: a contestant could
sweep the protection byte and read the reply instead of recovering the Data ID. The
only feedback is whether the car moved.

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

The three challenges are a deliberate progression through how a modern vehicle is
actually attacked: **talk to it, then trick it, then take it over.** Each one teaches
a technique that transfers directly to real cars, and each depends on the one before
it — not by an artificial gate, but because the later work genuinely needs what the
earlier work produced.

The car starts locked. Its operator console shows battery, 12 V state and network
status, but no raw bus traffic and no driving controls. The only way in is the
diagnostic connector, which is exactly the situation a technician or a researcher
faces with an unfamiliar vehicle.

---

### Challenge 1 — Discovery

*"There is a computer in this car. Find it and make it do something."*

**The real-world skill.** Every modern vehicle carries a diagnostic protocol called
UDS — Unified Diagnostic Services, ISO 14229 — the language behind every scan tool,
every dealer tester, and every emissions check. It is how a workshop reads a fault
code, how a VIN is confirmed without crawling under the windscreen, and how an
actuator is commanded during a service procedure. Learning to speak it unassisted,
without a commercial tool doing it for you, is the entry point to all vehicle
diagnostics work.

**What the contestant faces.** They connect an adapter to the diagnostic bus and see
*nothing*. This is correct and it is the first thing to understand: a diagnostic port
is not a firehose. Unlike the vehicle's internal networks, which chatter constantly,
a diagnostic bus is silent until something asks a question. There is no traffic to
capture and reverse-engineer here. You have to speak first.

**The work.** UDS is request–response. A tester sends a service identifier and the
ECU answers. The two that matter here:

- **ReadDataByIdentifier (`0x22`)** — "tell me the value of this named item." The
  names are 16-bit Data Identifiers, and a handful are standardised across the whole
  industry. `0xF190` is the VIN on essentially every car built in the last fifteen
  years. That is the contestant's first foothold: a real identifier, on a real
  protocol, returning a real value.

- **RoutineControl (`0x31`)** — "run a procedure." This is the service a workshop
  uses to bleed ABS brakes, cycle a window motor through its learn procedure, or
  sweep an actuator through its range. It is the part of UDS that *makes the car do
  things* rather than merely report on them.

Having found the VIN, the contestant sweeps for other identifiers and finds two more
in the manufacturer-specific block. Then they go looking for routines. The routine
identifiers sit in a small contiguous group, so finding one makes its neighbours the
obvious next probe — the puzzle is meant to be what the routines *do*, not a
brute-force search of 65,536 numbers.

**The payoff.** One of those routines is a pivot: the car spins in place, one wheel
forward and one back, through a fixed arc. It is unmistakable, it is physical, and
it happens because the contestant asked for it in the car's own language. The
routine's response carries the Challenge 1 code.

**Why the flag is in the response and not somewhere else.** UDS routines return a
"routine status record" — whatever the ECU wants to report about what it just did.
Putting the code there means the contestant has to correctly parse a real protocol
response, not just observe that something moved.

---

### Challenge 2 — Sessions, and the door that is not the door

*"The obvious way in is locked. Find the way that is actually open."*

**The real-world skill.** Diagnostic access is layered. An ECU boots in a default
session where it will answer harmless questions, and refuses anything consequential.
Ask it properly — `DiagnosticSessionControl (0x10)`, extended session — and a larger
surface opens up. This is the single most common stumbling block for people new to
UDS: they send a valid request, get a refusal, and conclude the feature does not
exist.

It does exist. The ECU told them exactly what was wrong, in a **negative response
code** — a one-byte reason attached to every refusal. Reading an NRC and acting on it
is the most transferable habit in diagnostics work, and it is what this challenge
teaches.

**What the contestant faces.** One of the identifiers found in Challenge 1 — the ECU
serial number, `0xF18C` — refuses to answer. The refusal is not a silence or an
error; it is `serviceNotSupportedInActiveSession`. The car is saying *"not like
this."* Change session, ask again, and it answers.

**The misdirection, and why it is fair.** Among the routines is one plainly named for
enabling diagnostic bridging. It looks like precisely what a contestant would want,
and it **always refuses** — `securityAccessDenied`, in every session, under every
condition. That door never opens.

The routine beside it is a self-test. It sounds like housekeeping. But an actuator
self-test genuinely needs the diagnostic tester to be able to command the actuators
while it runs, so for **five seconds** it opens a path from the diagnostic bus to the
vehicle's internal bus.

That contrast is the lesson: **one refusal says "wrong conditions", the other says
"never".** Distinguishing them is real diagnostic reasoning. And the security lesson
underneath it is one that recurs constantly in real vehicles — the dangerous
capability is rarely behind the door labelled with its name. It is a side effect of
something that sounds routine.

**The composition.** Now the contestant can inject onto the internal bus, but only
briefly, and only two message types. They cannot invent commands: the car's motor
controllers reject anything not carrying a correct **message protection field** — a
checksum keyed by a value that is never transmitted. This is not invented for the
challenge; it is AUTOSAR End-to-End protection, and it is on real safety-relevant
buses precisely to stop this kind of injection.

But they do not need to invent anything. Another identifier returns a **snapshot of
the last commands the car sent to each wheel, protection field intact.** Run the
pivot, read the snapshot, and you have a genuine, still-valid left-wheel command and
a right-wheel command.

The pivot drives the wheels in *opposite* directions. So the contestant takes the
captured frames and sends both wheels the one that drives them the *same* way. They
have composed a command the car has never issued, from parts it handed them, without
breaking any cryptography at all.

The car lurches sideways. It should not be able to do that while locked, and the
battery management module — watching the internal bus — notices and emits the
Challenge 2 code.

**Why the snapshot exists.** The gateway deliberately never forwards internal traffic
outward, so a contestant cannot simply listen and collect commands. Without a
sanctioned source of valid frames, Challenge 2 would collapse into passive sniffing
and replay. Making them *invoke* a routine and *read back* what it sent is the
difference between watching a car and interacting with one.

---

### Challenge 3 — Wireless takeover

*"Stop borrowing the car's own messages. Write your own."*

**The real-world skill.** This is the shape of every headline vehicle compromise of
the last decade: a network-facing service, credentials derived from data the vehicle
will tell you if you ask, and message protection that turns out to be a checksum
rather than a signature. Nothing here is exotic. That is the point.

**Finding the door.** The car runs a telematics service on its own WiFi. It is
discoverable two ways — a diagnostic identifier returns the host and port, and the
console mentions the interface once Challenge 2 is solved. Two routes deliberately:
a challenge whose entrance can only be found one way fails completely for a team that
misses that one way, and guessing a port number is not a skill worth testing.

**The credential.** The service wants a key. The key is derived from the vehicle's
own identity:

```
key = SHA-256( VIN : ECU serial : fleet salt ), first 16 hex characters
```

Every input is obtainable. The VIN came from Challenge 1. The serial required the
session work from Challenge 2. And the transform itself is **sitting in the console's
JavaScript**, served to any browser that loads the locked page.

**That is the vulnerability, and it is a real one.** Authentication logic shipped to
the client is among the most common findings in connected-vehicle assessments —
companion apps and in-car browsers routinely contain the algorithm that protects the
thing they talk to. Shipping it here is deliberate: it makes the challenge about
*recognising* that pattern rather than guessing at concatenation order, separators
and truncation. A contestant who views source has the answer; a contestant who does
not will not brute-force 64 bits.

It also enforces the ordering honestly. The transform is worthless without the VIN
and the serial, and the serial cannot be read without knowing how to change
diagnostic session. Challenge 3 needs Challenge 2's skill, not merely its flag.

**Now the protection matters.** Authenticated, the contestant has a raw path onto the
vehicle's internal bus — and this is where Challenge 2's borrowed frames stop being
enough. Replaying a captured command drives the car in a fixed arc at a fixed speed.
To actually *drive* it, they must construct commands the car has never sent, which
means producing a correct protection field, which means recovering the key that
generates it.

**What "recovering the key" actually means here**, and it is worth being precise
because the judging depends on it. The checksum is linear over GF(2). That means a
fixed-length key contributes a constant offset to every result, independent of the
message. A contestant does not recover *the* key — they recover a **256-member
equivalence class**, every member of which produces byte-identical checksums. Forgery
works perfectly without ever identifying the "true" value, and extra captured
messages give no further information. The real work is identifying the *algorithm* —
which checksum, over which bytes, in which order — not extracting a secret.

**The finish.** Driving the car under sustained forged commanding is a signature the
battery management module recognises, and it emits the Challenge 3 code. Redeeming it
unlocks full manual control of the vehicle from the console — which is, appropriately,
the same thing the attack achieved.

---

### Why the three cannot be short-circuited

The separation between Challenge 2 and Challenge 3 is **mechanical, not a policy
decision**. Challenge 2's composed attack reuses captured frames, which structurally
carry the speed the pivot routine uses. Challenge 3's detector only fires on a speed
that never appears in the car's own legitimate traffic. So no amount of replaying can
accidentally produce the Challenge 3 code — a contestant who has not learned to forge
cannot stumble into it, and one who has does not need to.

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

## 9. What is flashed where

| Target | Count | Unique? | Build |
|---|---|---|---|
| Pi image (`.wic`) | 8 | **identical** | `./build.sh` |
| `ttos-provision.conf` | 8 | **per car** | `provisioning/ttos-provision-carNN.conf` |
| BMS firmware | 8 boards | **identical binary** | `./build.sh <fqbn> TinytrekBMS` |
| Motor firmware | 16 boards | **identical binary** | `./build.sh <fqbn> TinytrekLMotor` / `RMotor` |

No build-time car id anywhere, and no per-car setup command. **The entire per-car
difference is one file on the Pi's boot partition** — drop it, power on, and the car
provisions itself and its three nodes.

---

## 10. Known gaps

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
