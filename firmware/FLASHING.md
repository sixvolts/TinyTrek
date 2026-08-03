# Flashing the CTF node firmware

Three nodes per car. This package builds them in two modes.

```
TinytrekLMotor   left motor    QT Py SAMD21 + MCP2515      CAN id 0x111
TinytrekRMotor   right motor   QT Py SAMD21 + MCP2515      CAN id 0x113
TinytrekBMS      BMS           Feather RP2040 CAN          0x115 in / 0x116 out
```

---

## No secrets in this archive

Nothing here is confidential and nothing is per-car. The nodes receive their Data
IDs, unlock codes and detector thresholds **from the Pi over CAN during
provisioning**, and store them in flash.

---

## Flashing a node

Open the sketch in the Arduino IDE, pick the board, upload. That is all.

```
TinytrekLMotor/TinytrekLMotor.ino    →  Adafruit QT Py SAMD21   (left motor)
TinytrekRMotor/TinytrekRMotor.ino    →  Adafruit QT Py SAMD21   (right motor)
TinytrekBMS/TinytrekBMS.ino          →  Adafruit Feather RP2040 CAN
```

**No libraries to install and nothing to configure.** Each sketch folder carries the
library sources it needs — the CAN driver, the NeoPixel driver, and on the motors the
SAMD flash-as-EEPROM code — so the IDE compiles them as part of the sketch. That is
also why the includes are quoted rather than angle-bracketed: the IDE only searches
the sketch folder for quoted includes.

You do need the board support packages, from Boards Manager:

- **Adafruit SAMD Boards** for the QT Py motors
- **Raspberry Pi Pico/RP2040** (Earle Philhower) for the Feather CAN BMS

`build.sh` batch-builds all three from a terminal if you would rather not click
through the IDE three times per car. It is optional and nothing depends on it.

**Three binaries for the entire fleet** — the same left-motor, right-motor and BMS
images go on all eight cars. No per-car build, no car id anywhere.

### What happens after you flash

A freshly flashed node has **nothing stored**, so it is permissive: it accepts the
unprotected drive command and the car drives normally. That is deliberate — a car
that will not move until it has been provisioned is useless during setup.

The Pi provisions the nodes **automatically, on its first boot with a provisioning
file present**. It bursts the values for about six seconds, the nodes write them to
flash, and it never repeats. Nothing is transmitted at runtime after that: the Data
ID is what Challenge 3 exists to recover, and it has no business being on the bus
while a contestant is on it.

A node that has stored its values **ignores further config frames**, so nobody who
reaches the drive bus can overwrite them. If you replace a node later, run
`sudo ttos-provision-nodes` on that car to push the values again.

---

## What the firmware does

**Motors** — a drive command must be the 8-byte protected form,
`[steps u32 BE][dir][rpm][nonce][crc8]`, with `crc8` over bytes 0..6 keyed by this
motor's compiled-in Data ID. Anything else is dropped.

**Rejection is absolutely silent**: no error frame, no reply, no serial print, no
LED change. That is deliberate — any observable difference between "rejected" and
"never arrived" is an oracle that would let a contestant brute-force the CRC byte by
watching for the frame that stops being ignored.

**It is also why a mismatch is hard to debug.** If the Pi and the motor disagree by
one byte, the car simply does not move and nothing anywhere says why. Flash ONE car
first and confirm it drives before doing the other seven -- and since all eight are
built from the same constants now, proving one proves the build.

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

1. **It still drives** from the control pad, provisioned or not. Motors are
   baseline, so this should be unaffected -- if it is not, something else changed.
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
