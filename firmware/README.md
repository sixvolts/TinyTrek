# TinyTrek node firmware

The Arduino/MCP2515 CAN nodes on the car's **internal drive bus (`can0`, classic
CAN 2.0 @ 500 kbit)**:

| Sketch | CAN ID | Role |
|---|---|---|
| `TinytrekLMotor` | `0x111` | left stepper: `[steps:uint32 BE][dir]` — move N steps then stop; dir `0x02`=reverse else forward |
| `TinytrekRMotor` | `0x113` | right stepper: same format |
| `TinytrekBMS`    | `0x115` | power/12V rail: `[0x01]`=on, `[0x02]`=off |

The dashboard's control pad drives these verbatim (see `meta-tt-ctf/recipes-apps/ttos-dashboard`).

## Building (vendored library — no global install)

The `CAN.h` dependency (Sandeep Mistry `arduino-CAN`, MIT) is **vendored** in
`libraries/CAN/`, so builds don't depend on whatever is installed globally.

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
- **Motor speed / torque**: `int rpm = 150;` in each motor sketch sets the stepper
  speed. Lower it (e.g. 80–100) if it's too fast / torque-y on smooth floors. This
  needs a reflash of both motor nodes.
- **`TinytrekBMS`** uses `CAN.setPins(PIN_CAN_CS, PIN_CAN_INTERRUPT)` — those macros
  are board-specific; set literal CS/INT pins (like the motor sketches' `setPins(3, 0)`)
  to match your MCP2515 wiring if they aren't defined for your board.
- **Battery sense**: a 120 kΩ / 40.2 kΩ (0.5%) divider on the 12 V pack feeds BMS
  `A1` (ratio 0.2509; `Vbat = Vadc × 3.985`). `TinytrekBMS` reads it and transmits
  `Vbat` (mV, uint16 BE) on `0x116` at ~1 Hz. `ADC_REF_MV = 3300` (3.3 V board); if
  the gauge is slightly off vs a multimeter, nudge that constant.
