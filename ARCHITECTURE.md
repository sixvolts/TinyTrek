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
            candrive    │                          │  candiag
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

**The interfaces are named for their role: `candrive` and `candiag`**, pinned to
SPI address by `/etc/udev/rules.d/10-ttos-can.rules`. `spi0.0` is the HAT's CAN0
terminal (the motor and BMS harness); `spi1.0` is the CAN1 terminal (the contestant
diagnostic port). The device-tree overlay fixes that mapping, so it is identical on
every car.

**They are not called `can0`/`can1` for a reason that cost days.** Left to the
kernel, the two MCP2518FD controllers are named in whatever order they probe, and
that order RACES — the same car, physically unchanged, came up with the drive
harness on `can0` one boot and `can1` the next. Every symptom of that looks like a
wiring fault, and it produced a bus-role claim recorded as "verified on hardware"
that was simply a snapshot of one boot's dice roll.

Two things had to be fixed. The `.link` files that were supposed to pin the names
could never have worked: they match on `Path=`, which compares udev's `ID_PATH`, and
`ID_PATH` is empty for SPI-attached CAN controllers — `networkctl` said
`Link File: n/a` on every car. And renaming to `can0`/`can1` cannot work either,
because udev will not swap two interfaces into each other's names; the rename fails
silently and leaves the racy order. Role names avoid the collision entirely, and a
config that says `candrive` cannot be quietly pointing at the diagnostic bus.

| | DRIVE (`candrive`) | DIAG (`candiag`) |
|---|---|---|
| Speed | classic CAN 2.0, 500 kbit | **CAN FD**, 500 kbit arb / 1 Mbit data |
| Who is on it | 2 motors, BMS, Pi | Pi, and a contestant's adapter |
| Idle traffic | heartbeat + beacon, ~23 frames/s | **silent** until a request arrives |

`candrive` is classic because the motor nodes are MCP2515, which has no FD support at
all — an FD frame there is a form error that drives them error-passive and then
bus-off, stopping propulsion. FD is disabled in configuration so the Pi cannot emit
one even by mistake.

`candiag` is FD because responses do not fit otherwise: VIN 20 bytes, pivot response
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

**Codes are write-once; detector TUNING is not.** The BMS accepts record type 0x20
(pivot rpm, pair window, C3 count and window) even when already provisioned, and
persists it. The whole config handler used to be refused once configured, which
meant a pivot-rpm retune silently could not reach the node: the burst went out, the
tool reported success, and the BMS kept the old value. That divergence is not
cosmetic — C3's discriminator is *same direction AND an rpm the pivot never uses*,
so a Pi pivoting at 150 against a BMS still believing 75 lets a team replay captured
pivot frames straight into the C3 code with no forgery at all. Leaving tuning open
is safe because 0x101 is refused by the relay's SEND path and is not in the C2
bridge allowlist, so no contestant-reachable path can inject it.

**A node reports what it holds.** The BMS prints a `cfg:` line on its serial port
every 5 s — configured or not, a fingerprint of the stored codes (not the codes
themselves; contestants can reach that USB port), and the tuning values. Boards on
one fleet share codes, so they must share a fingerprint; an odd one out is a board
to erase. It is periodic rather than printed at boot because these are native-USB
boards and a reset drops the CDC connection before a monitor can attach.

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
cangw -A -s candrive -d candiag -f 7D1:7FF     flag frames out
cangw -A -s candrive -d candiag -f 7D2:7FF
```

**Nothing else crosses in either direction.** Drive traffic must not go outbound or
contestants could build the C2 corpus by passive sniffing, which would collapse C2
from "interact with the car, then compose" into "watch and replay". The sanctioned
corpus source is the snapshot DID instead.

The C2 inbound bridge is *not* a cangw rule — it is Go, because installing kernel
gateway rules needs `CAP_NET_ADMIN` and `ttos-dashboard` is the one process
contestants can reach over the network.

**One writer, one bus.** Every frame the Pi puts on the drive bus — heartbeat, node
config, the C1 pivot, the C2 bridge, the C3 relay and the operator's control pad —
goes through a single socket. A second, config-gated socket existed for the control
pad until 2026-08-04; provisioning emptied that config on every competition car, so
the gate disabled nothing except the operator's own panel, including the e-stop.

**The 12 V rail is part of the sanctioned path.** The stepper drivers are dead
without it and the BMS boots with it off, so the C1 routine energizes it (`0x115`
`[0x01]`) *before* commanding the wheels. Nothing else in shipping code did, which
meant a freshly booted car ran a perfectly valid pivot into unpowered drivers and
returned the flag anyway — a solve with a motionless robot.

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

**A reboot returns a car to LOCKED.** Every piece of challenge state — session
tiers, the car-level unlock record, the C2 bridge window — is in memory and nothing
persists it, so power-cycling is a full reset. A cookie held from before the reboot
is worthless: the client still has it, but the server has forgotten the token. The
only thing written to disk is the node-provisioning marker, which is not challenge
state. Verified on hardware, and asserted by `test-panel.py --reboot` (P9) so that it
stays true if anyone later persists state to survive a crash.

**There is no judging endpoint.** One existed briefly and was removed: it was never
asked for, it was unauthenticated on the AP so it handed contestants a
solved/not-solved readout, and the "sequence number" it reported was the relay's log
line counter dressed up as something a judge would verify against. Judging is a team
showing you the code they recovered.

The car-level record itself stays, because it is not a judging feature — it is what
`armMask()` reads.

**Reset:** `sudo ttos-reset` on the car clears every unlock, re-arms the detectors,
and reloads the gateway. Runnable from the serial console with no laptop, because
between rounds that is all an operator has. **It verifies before it declares.** It
used to print "This car is LOCKED and ready for the next team" and exit 0
unconditionally — so a failed gateway reload handed the next team a car that boots,
serves the panel, answers every diagnostic request and pivots correctly, and can
never award a code, because `0x7D1`/`0x7D2` no longer have a route to the
diagnostic bus. It now checks the dashboard, the gateway and the rule count, and
exits non-zero with the diagnosis if any of them is wrong.

**Stopping a car is electrical.** The motor sketches drain their step buffer on
`stepsLeft > 0` alone; `heartbeatOK()` drives the status LED and nothing else. Pull
the Pi, kill the service or cut the bus and the wheels finish what they have — up to
250 steps, which at an attacker-chosen `rpm=1` is 75 seconds. Do not design anything
around heartbeat loss halting the wheels; that claim was written into three shipping
places and was never true. The e-stop therefore drops the 12 V rail **first**, ahead
of both motor frames, and is best-effort across all three rather than stopping at
the first transmit error — the error case is a contestant saturating the drive bus
through the relay, which is exactly when someone reaches for the button.

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

**Tier 3 is the whole of drive authority — there is no config switch beside it.**
The dashboard has exactly one writer to the DRIVE bus, shared with the heartbeat,
node provisioning, the UDS routines, the C2 bridge and the C3 relay. A second,
config-gated socket for the control pad existed until 2026-08-04 and was removed:
provisioning emptied `TTOS_DASH_DRIVE` on every competition car, which read as a
read-only safety gate but gated nothing else on that bus, so the only thing it
disabled was the operator's own panel — **including the e-stop**, whose frames
were discarded while the handler still answered `{"ok":true}`.

The e-stop (`stop`) is exempt from the tier gate by design: it must work from a
locked panel, a lapsed session, or a judge's phone. It closes the C3 bridge, zeroes
both step buffers and drops the 12 V rail. `/api/control` now reports failure when
frames do not reach the bus, and `test-panel.py` asserts the **wire** (P6c, P6d),
not the HTTP status — an HTTP 200 is not propulsion.

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

That file must be named exactly `ttos-provision.conf`; the car checks
`/boot/firmware/` and `/boot/` and nowhere else. A file under any other name is not
found and the car boots into factory mode with an open `TTOS-TEST` AP and no
challenge identity, silently. `render-fleet.py` emits `flash/car-NN/` directories
with the file already correctly named so there is nothing to rename while
flashing.

It also emits `TTOS_SSH_AUTHORIZED_KEY`. A fresh card wipes the rootfs and with it
any key installed by hand on the previous one, and every bench script uses
`ssh -o BatchMode=yes` — so without this a reflashed car is unreachable to the whole
harness until someone copies a key over, eight times, after every reimage. When no
key is configured the generator writes a comment saying so rather than silently
omitting the line: an event car should not carry a build machine's key, and that
needs to be a visible decision rather than an accident.

---

## 10. Known gaps

- **Motor stutter unexplained.** Bus contention is ruled out with data measured on
  the drive controller itself (0.34% load, zero bus errors, command-stream stdev
  1.5 ms over 160 frames). Prime suspect is the browser-driven keepalive over WiFi,
  still unmeasured. Note the earlier version of this measurement read its error
  counters from the wrong interface and reported zeros for a controller it never
  opened; the numbers above are from the corrected tool.
- **Pivot at 75 rpm is unconfirmed mechanically.** 250 pps should clear the
  resonance band; only a real drivetrain can say whether it is smooth.
- **One team can disarm another car's detectors.** Codes are fleet-wide, PSKs are on
  placards in the same room, and `/api/flag` needs no prior tier — so a team can
  paste a code into a neighbouring car's panel and stand its detectors down. The
  victim then solves C2 correctly and sees nothing. Recoverable with `ttos-reset`,
  so the cost is a lost round rather than a lost event, and it requires deliberate
  sabotage. It is the one place one team's action reaches another team's challenge,
  and it is a consequence of the fleet-wide decision in section 3 rather than a bug.
- **A C1 solve leaves no trace outside journald.** The car-level record tracks C2 and
  C3 only, because its job is driving the arm mask and C1 has no detector. Nothing
  reads it out, by design.
- **Placards not written.** SSID, PSK, car id, the DIAG bus parameters, "CAN FD
  required", the panel URL, and "use Safari or Firefox — Chrome force-upgrades
  port 80".
- **No judging brief for the event.** `judge-packet.md` was deleted 2026-08-04
  rather than repaired: it listed per-car unlock codes after the fleet went uniform,
  and it named an exact Data ID when a correct Challenge 3 solve recovers a
  256-member equivalence class and cannot identify the true value. `WALKTHROUGH.md`
  section 4 has the substance if a printed sheet is wanted, including what a correct
  C3 solve can and cannot be asked to produce.
- **The remaining six cars are unflashed.** Cars 01 (bench DUT) and 02 are on the
  current image and firmware.

---

## 11. Walkthrough

`WALKTHROUGH.md` is the operator's solve of all three challenges end to end, with
the reasoning at each step. It is the fastest way to check that a car is genuinely
solvable, and it is the reference for what a contestant is expected to do.
