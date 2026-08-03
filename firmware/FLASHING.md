# Flashing the CTF node firmware

Three nodes per car. This package builds them in two modes.

```
TinytrekLMotor   left motor    QT Py SAMD21 + MCP2515      CAN id 0x111
TinytrekRMotor   right motor   QT Py SAMD21 + MCP2515      CAN id 0x113
TinytrekBMS      BMS           Feather RP2040 CAN          0x115 in / 0x116 out
```

---

## ⚠ THIS ARCHIVE CONTAINS SECRETS

`provisioning/firmware-constants.h` holds **every car's Data IDs and C2/C3 unlock
codes**. Recovering a Data ID *is* Challenge 3. Keep this off the competition floor,
off shared machines, and delete it when the fleet is flashed.

---

## Two build modes

**Baseline** — no `TTOS_CAR` set. Message protection and detection compile out
entirely. The car drives normally. Use this for drivetrain debugging, and for any
node you are not yet ready to lock down.

```sh
./build.sh <fqbn>                      # all three
./build.sh <fqbn> TinytrekLMotor       # one
./build.sh <fqbn> TinytrekLMotor /dev/ttyACM0   # build + upload
```

**Challenge** — `TTOS_CAR` set to a two-digit car id. Per-car constants are staged
from `provisioning/firmware-constants.h` into each sketch directory as
`ttos-fleet.h`, and `-DTTOS_CAR_NN -DTTOS_CHALLENGE=1` is passed to the compiler.

```sh
TTOS_CAR=01 ./build.sh <fqbn>
TTOS_CAR=01 ./build.sh <fqbn> TinytrekBMS /dev/ttyACM0
```

**The staged `ttos-fleet.h` copies are secrets too.** Delete them when you are done:
`rm -f firmware/*/ttos-fleet.h`.

---

## FQBNs

Use your board's actual FQBN. What I compile-tested with:

```
motors   arduino:samd:mkrzero                  (SAMD21 proxy — see below)
BMS      rp2040:rp2040:adafruit_feather_can
```

The motor nodes are **Adafruit QT Py SAMD21**; the correct FQBN for them is
`adafruit:samd:adafruit_qtpy_m0` once the Adafruit SAMD core is installed. I used
`arduino:samd:mkrzero` because it is the same architecture and toolchain and was
available — it proves the code compiles for SAMD, **not** that the QT Py pin
definitions resolve. If `PIN_NEOPIXEL` or the CAN pins fail to compile on the real
FQBN, that is why, and it is a board-variant issue rather than a logic one.

If your MCP2515 boards use an 8 MHz crystal rather than 16:

```sh
TTOS_CAR=01 ./build.sh <fqbn> TinytrekLMotor \
  --build-property "build.extra_flags=-DCAN_CLOCK_HZ=8000000"
```

---

## What the challenge build changes

**Motors** — a drive command must be the 8-byte protected form,
`[steps u32 BE][dir][rpm][nonce][crc8]`, with `crc8` over bytes 0..6 keyed by this
motor's compiled-in Data ID. Anything else is dropped.

**Rejection is absolutely silent**: no error frame, no reply, no serial print, no
LED change. That is deliberate — any observable difference between "rejected" and
"never arrived" is an oracle that would let a contestant brute-force the CRC byte by
watching for the frame that stops being ignored.

**It is also why a mismatch is hard to debug.** If the Pi and the motor disagree by
one byte, the car simply does not move and nothing anywhere says why. Flash ONE car
first and confirm it drives before doing the other seven.

**BMS** — becomes the drive-bus monitor. It watches `0x111`/`0x113`, and emits
`0x7D1` (C2 code) or `0x7D2` (C3 code) at 2 Hz while a detection condition holds:

- **C2**: both motors commanded the *same* direction within 250 ms. No legitimate
  interface does that while the panel is locked — the pivot always drives the wheels
  in opposite directions.
- **C3**: 15 consecutive same-direction commands within 3 s at an rpm that is **not**
  `PIVOT_RPM` (75).

Both are gated by the arm mask in heartbeat byte 1, and any failure — stale
heartbeat, short frame, no heartbeat — leaves them **disarmed**. A dead station is
recoverable; a silently leaked flag is not.

### PIVOT_RPM must match the Pi

`PIVOT_RPM = 75` in `TinytrekBMS.ino` must equal `TTOS_PIVOT_RPM` in
`/etc/default/ttos-dashboard`. If they diverge, **the car fires `0x7D2` on its own
pivot routine and leaks the C3 code before anyone attempts the challenge.**

75 is provisional. It clears the steppers' mid-band resonance (50 rpm = 167 pps sits
inside it and drops steps) and is not a speed anyone would teleoperate at — that
second property is what keeps a C2 replay from reaching C3. If a real car still
grinds at 75, try 150 before going back to 50, and change it in **both** places.

---

## After flashing car 01, check these in order

1. **It still drives.** Baseline behaviour must survive. If it does not, the CRC
   disagrees — see above, it will fail silently.
2. **The C1 pivot turns ~45° and does not grind.** This is the mechanical half of the
   75 rpm decision and the only place it can be answered.
3. **`0x7D1` on a same-direction translation**, `0x7D2` on sustained forged
   commanding. Watch on the diagnostic bus; the gateway forwards both.
4. **Normal driving emits neither** — with the panel unlocked the arm mask is 0 and
   the detectors are stood down.

Known gap to expect: the panel's own drive controls currently send the **6-byte
legacy frame**, which challenge motor firmware rejects. Fix is pending on the Pi
side. Until it lands, use the pivot routine or the C3 relay to move a
challenge-flashed car, not the control pad.
