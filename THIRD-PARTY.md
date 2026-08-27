# Third-party components

The MIT license at the root covers the original work in this repository. It does
**not** cover the third-party components vendored into the tree — copied in
rather than pulled by a package manager — which each keep their own license,
including LGPL-licensed Arduino libraries under `firmware/` and an OFL-licensed
font in the dashboard's web assets. They are listed here so [LICENSE](LICENSE)
is not mistaken for a blanket claim over the whole tree.

Nothing here is modified from upstream unless the notes say so.

## Arduino libraries (`firmware/`)

The three sketch folders — `TinytrekBMS/`, `TinytrekLMotor/`, `TinytrekRMotor/` —
each carry a flat copy of their libraries, because the Arduino IDE compiles a
sketch folder as a unit. The same library therefore appears up to three times.

| Component | Files | Upstream | License |
|---|---|---|---|
| Adafruit NeoPixel | `Adafruit_NeoPixel.{cpp,h}`, `Adafruit_Neopixel_RP2.cpp`, `esp.c`, `esp8266.c`, `kendyte_k210.c`, `rp2040_pio.h` | [adafruit/Adafruit_NeoPixel](https://github.com/adafruit/Adafruit_NeoPixel) | **LGPL-3.0-or-later** |
| arduino-CAN | `CAN.h`, `CANController.{cpp,h}`, `MCP2515.{cpp,h}`, `ESP32SJA1000.{cpp,h}` | [sandeepmistry/arduino-CAN](https://github.com/sandeepmistry/arduino-CAN) | MIT (© Sandeep Mistry) |
| FlashStorage / FlashAsEEPROM | `FlashStorage.{cpp,h}`, `FlashAsEEPROM.{cpp,h}` (SAMD21 motor nodes only) | [cmaglie/FlashStorage](https://github.com/cmaglie/FlashStorage) | **LGPL-2.1-or-later** (© Arduino LLC) |

The LGPL components are used as libraries linked into the node firmware and are
redistributed here unmodified, with their license headers intact.

## Web assets (`meta-tt-ctf/recipes-apps/ttos-dashboard/files/src/cmd/dashboard/web/`)

| Component | File | Upstream | License |
|---|---|---|---|
| Shojumaru | `shojumaru.ttf` | [Google Fonts](https://fonts.google.com/specimen/Shojumaru) | SIL Open Font License 1.1 |

The font is bundled rather than linked because the car serves its panel from an
isolated WiFi AP with no route to the internet.

## Yocto layers (not in this repository)

`poky/`, `meta-openembedded/`, `meta-raspberrypi/` are cloned into the repo root
at build time by `kas` and are gitignored. They are upstream projects under their
own licenses (MIT for poky and meta-raspberrypi metadata; the built image's
package licenses are recorded in the Yocto license manifest under the deploy
directory).
