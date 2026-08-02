# TinyTrek node firmware

The CAN nodes on the car's drive bus (classic CAN 2.0 @ 500 kbit). **On the current
hardware this bus enumerates on the Pi as `can1`, not `can0`** — verified 2026-08-01
(motors + BMS answer there; `can0` has no nodes and writes fail with `ENOBUFS`).

Boards: motor nodes are **Adafruit QT Py SAMD21 + MCP2515** (`CAN.setPins(3, 0)`);
the BMS is an **Adafruit Feather RP2040 CAN** (built-in MCP2518FD).

| Sketch | CAN ID | Role |
|---|---|---|
| `TinytrekLMotor` | `0x111` | left stepper (consumed step buffer): `[steps:uint32 BE][dir][rpm]` — ADDS `steps` to a capped buffer the node drains as it steps; dir `0x02`=reverse / `0x01`=forward / `0x00`=stop (clear); `rpm` sets speed. |
| `TinytrekRMotor` | `0x113` | right stepper: same format |
| `TinytrekBMS`    | `0x115` | RX power command: `[0x01]`=12V on, `[0x02]`=off |
| `TinytrekBMS`    | `0x116` | TX **beacon** @5 Hz: `[v_hi][v_lo][pwr][flags]` — pack mV (uint16 BE), `pwr` `0x01`=rail on / `0x02`=off (the **actual** state, not the commanded one), `flags` bit0 = low-voltage cutoff latched |
| *(the Pi)*       | `0x100` | TX **heartbeat** @5 Hz: `[seq][flags]` — "the Pi is alive and in control". `flags` bit0 = C2 detector ARMED, bit1 = C3 detector ARMED (see below) |

The dashboard's control pad drives these verbatim (see `meta-tt-ctf/recipes-apps/ttos-dashboard`).

## Status LEDs — reading the system state off the robot

Every node's onboard NeoPixel reports the health of the **whole chain**, so a
glance tells you which link is broken. Both periodic messages (`0x100`, `0x116`)
run at 5 Hz with a 1 s timeout on the motor nodes.

**Motor nodes** — two things must be true to leave red: a Pi heartbeat *and* a BMS
beacon saying the 12 V rail is on.

| LED | Meaning |
|---|---|
| 🔵 **solid blue** | **CAN controller never initialised** — board/SPI fault, retrying |
| 🔵 **blink blue** | controller up, but **not one frame ever received** — bus/wiring |
| 🟢 solid green | **READY** — heartbeat + 12 V both good, not moving |
| 🟡 solid yellow | **ACTIVE** — propulsion commanded and running |
| 🔴 **double**-blink red | no heartbeat from the Pi (12 V is fine) |
| 🔴 **regular** blink red | no 12 V active (heartbeat is fine) |
| 🔴 solid red | both missing |

So: *blue means this node can't talk at all; red means it can hear the bus but the
system isn't ready; blink pattern says which signal is missing.*

**Blue is a board fault, red is a system fault.** Solid blue will not be fixed by
any amount of cable work — the MCP2515 isn't answering over SPI. Blinking blue is
the opposite: the controller is fine and nothing is reaching it, so check wiring,
termination and bit rate. A healthy bus always carries the BMS beacon at 5 Hz, so
"never received a frame" is always a real fault.

### Why the blue states exist

They were added on 2026-08-02 after a car burned an afternoon. `CAN.begin()` failed
at power-on, the sketch printed to a serial port nobody was attached to and **carried
on anyway**, and the node sat in configuration mode forever. In that state the
controller is electrically passive: it neither acknowledges frames nor corrupts the
bus. From the Pi everything looked healthy — another node was providing the ACK, so
transmits succeeded and the error counters stayed at zero — while the motor node
heard nothing. On the robot it showed as solid red, identical to a disconnected bus.

Two fixes came out of it, both in the sketch rather than the vendored library:

- **Settle, then retry.** The MCP2515 needs its oscillator running before it answers
  over SPI (128 osc cycles plus crystal settling, up to milliseconds from cold), but
  the library's `reset()` waits only 10 µs. So `begin()` can lose that race at cold
  power-on and win it on a warm reset. The sketch now waits 50 ms before its first
  attempt and **retries every second until it succeeds**, non-blocking.
- **SPI at 4 MHz** instead of the library's 10 MHz default, which is the MCP2515's
  absolute maximum and marginal over hand-wired jumpers.

`begin()` does verify SPI communication (it reads `CANCTRL` back twice), so a
success genuinely means the controller is there. It does **not** verify the crystal:
bit timing is chosen from whatever `CAN_CLOCK_HZ` claims. An 8 MHz module told 16 MHz
reports success and then runs at half rate — deaf, and it corrupts the bus with error
frames. That last part is diagnostic: a wrong-crystal node *dirties* the bus, so if
the Pi's error counters are clean the problem is not bit rate.

For 8 MHz modules, build with:

```bash
./build.sh <fqbn> TinytrekLMotor --build-property "build.extra_flags=-DCAN_CLOCK_HZ=8000000"
```

### Serial status

Each node prints its state every 2 s — `CAN=up rx=yes hb=ok 12v=on steps=0`, or the
reason it is down. Deliberately periodic, not once at boot: these are native-USB
boards, so **a reset drops the CDC connection** and anything printed during `setup()`
is gone before a monitor can reattach. That is precisely why the original one-shot
"Failed to initialize CAN BUS." message was never seen by anyone.

The heartbeat is transmitted **unconditionally** by the CTF service layer. It used
to ride the dashboard's drive gate, so a read-only car transmitted nothing and its
motor nodes double-blinked red by design — that is no longer true, and a CTF car
must keep the heartbeat alive while its panel is locked or no challenge can move
the wheels. A car showing double-blink red now means the Pi really is not talking.

### Heartbeat `flags` byte — detector arm mask

`0x100` byte 1 tells the **BMS** which of its detectors should be armed:

| Bit | Meaning when SET |
|---|---|
| 0 (`0x01`) | C2 detector armed (watch for same-`dir` on `0x111`/`0x113`) |
| 1 (`0x02`) | C3 detector armed (watch for sustained forged commanding) |

A locked car sends `0x03`; each bit clears when that challenge's code is redeemed
on this car, so an unlocked panel driving normally does not re-trigger detection.

**Positive arming, deliberately.** With "redeemed" semantics `flags == 0` would
mean *armed*, so a Pi bug, a short frame, or mismatched firmware would leave
detection live and the car would leak its C2 code during ordinary driving. With arm
semantics those same failures leave detection **disarmed** — the challenge does not
fire, someone reports a broken station, and it gets fixed. A dead station is
recoverable; a leaked flag is not. So the BMS must treat a stale heartbeat (>1 s,
the same timeout the motor nodes use) or a DLC < 2 as **arm mask 0**.

The Pi re-reads the mask every tick, so the BMS *mirrors* current state rather than
latching an event: a dropped frame self-corrects in 200 ms, a node that power-cycles
re-syncs with no re-announcement, and restarting the service re-arms both detectors.

## Building (vendored libraries — no global install)

Dependencies are **vendored** under `libraries/`, so builds don't depend on
whatever is installed globally:

- `libraries/CAN/` — Sandeep Mistry `arduino-CAN` (MIT), the `CAN.h` API.
- `libraries/Adafruit_NeoPixel/` — Adafruit NeoPixel (LGPL-3.0), the status LEDs
  (all three nodes). QT Py SAMD21 uses `PIN_NEOPIXEL` = 11 and has no
  `NEOPIXEL_POWER` gate pin; the Feather RP2040 CAN has both. The sketches guard
  on `#ifdef`, so the same code is correct on either board.

**With `arduino-cli`:**
```bash
./build.sh arduino:avr:uno                    # all three
./build.sh arduino:avr:uno TinytrekBMS /dev/cu.usbmodemXXXX   # build + upload one
```
The script passes `--libraries ./libraries` so the compiler uses the vendored copy.

**With the Arduino IDE:** point it at the vendored library once, then open the
sketches normally:
```bash
ln -s "$(pwd)/libraries/CAN" ~/Documents/Arduino/libraries/CAN
```

## Notes / tunables

- **FQBN**: set to your actual node board. The nodes are **RP2040** (3.3 V) with a
  built-in MCP2515 — the sketch's `PIN_CAN_CS`/`PIN_CAN_INTERRUPT` come from the
  Adafruit Feather RP2040 CAN variant (`rp2040:rp2040:adafruit_feather_can` in the
  earlephilhower core). (The "Arduino UNO" header comment is a stale leftover.)
- **Consumed step buffer (smooth + loss-tolerant) + speed byte**: each command
  ADDS its `steps` to a capped buffer (`STEPS_MAX`, 250 — ~2.5× the dashboard's
  per-frame chunk) that the node drains as it steps, staying energised while the
  buffer holds steps. Motion follows the buffer, not message timing, so dropped or
  jittered frames don't stall it — it keeps stepping off the reservoir. `rpm` sets
  speed; the dashboard adds `TTOS_DASH_STEP_CHUNK` (100) steps every ~150 ms
  keepalive — about 2× what's consumed between frames, so the buffer fills to the
  cap and rides a couple of missed frames — and a release/stop sends `dir 0x00`
  to clear it promptly. The cap bounds coast and
  makes runaway impossible (it halts within `STEPS_MAX` steps if commands stop).
  Bigger `STEPS_MAX` = smoother through heavier loss but longer coast if a release
  is missed. `int rpm = 100;` is only the fallback speed when a frame omits the
  `rpm` byte.
- **`TinytrekBMS`** uses `CAN.setPins(PIN_CAN_CS, PIN_CAN_INTERRUPT)` — those macros
  are board-specific; set literal CS/INT pins (like the motor sketches' `setPins(3, 0)`)
  to match your MCP2515 wiring if they aren't defined for your board.
- **Battery sense**: a 120 kΩ / 40.2 kΩ (0.5%) divider on the 12 V pack feeds BMS
  `A1` (ratio 0.2509; `Vbat = Vadc × 3.985`). `TinytrekBMS` reads it and transmits
  `Vbat` (mV, uint16 BE) on `0x116` at ~1 Hz. `ADC_REF_MV = 3300` (3.3 V board); if
  the gauge is slightly off vs a multimeter, nudge that constant.

## BMS status LED (onboard NeoPixel)

`TinytrekBMS` drives the Feather RP2040 CAN's onboard NeoPixel as a state light.
**Blink rate = 12 V relay; color = battery band; fast blink is reserved for the
low-voltage cutoff.** Bands come from the same 3S range as the dashboard gauge
(8.4 V = 0 %, 12.3 V = 100 % → 50 % = 10.35 V, 25 % = 9.375 V; cutoff 8.5 V):

| Battery band | 12 V **off** (solid) | 12 V **on** (slow blink) |
|---|---|---|
| ≥ 50 % | 🟢 solid green | 🟢 slow-blink green |
| 25–50 % | 🟠 solid amber | 🟠 slow-blink amber |
| < 25 % (above cutoff) | 🔴 solid red | 🔴 slow-blink red |
| below cutoff (8.5 V) | 🔴 **fast-blink red** (12 V forced off) | — |

Timing/brightness knobs: `BLINK_SLOW_MS` / `BLINK_FAST_MS` / `LED_BRIGHTNESS`.
The pixel is on `PIN_NEOPIXEL` (from the board variant); `NEOPIXEL_POWER` is
enabled if the variant defines it. Update is non-blocking (throttled millis
blink, `show()` only on change) so it never stalls CAN RX or the cutoff loop.
