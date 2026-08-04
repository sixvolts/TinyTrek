# provisioning/ — CTF fleet data (8 cars)

**`fleet-table.csv` is the single source of truth.** Everything else here is either
derived from it or generates it. Verified against `firmware-constants.h` and
`OPERATOR-SECRETS.md` on 2026-08-02 — all 8 cars consistent.

## Files

| File | Tracked in git? | What it is |
|---|---|---|
| `fleet-table.csv` | **no** | Master table. VIN, ECU serial, Data IDs, unlock codes, PSK, console hash |
| `firmware-constants.h` | **no** | Per-car `TTOS_DATAID_L/_R`, `TTOS_CODE_C2/_C3`, `TTOS_PIVOT_STEPS`. Included by node firmware |
| `ttos-provision-carNN.conf` ×8 | **no** | Per-car file, for reading and diffing |
| `flash/car-NN/ttos-provision.conf` ×8 | **no** | **Flash-ready.** Copy this one to the FAT boot partition |
| `OPERATOR-SECRETS.md` | **no** | Console passwords, plaintext. Never goes to the floor |
| `gen-fleet.py` | yes | The generator. **Do not run** — see below |
| `render-fleet.py` | yes | Deterministic re-render from the CSV. Safe to run |

Nothing here is committed except the two scripts, matching the repo's existing
"never commit provisioning data" rule in `.gitignore`. **Back this directory up
somewhere off the repo** — losing `fleet-table.csv` means reflashing every node.

## Flashing: use `flash/car-NN/`, and do not rename

The car checks exactly two paths and nothing else:

```
/boot/firmware/ttos-provision.conf
/boot/ttos-provision.conf
```

A file under any other name is not found, and the car boots into FACTORY mode --
open `TTOS-TEST` AP, `ttos`/`ttos`, no challenge identity -- with no error anywhere
saying why. That is a silent failure that costs a reflash to notice, so
`render-fleet.py` emits `flash/car-NN/ttos-provision.conf`: one directory per car,
every file already named what the car looks for, nothing to rename while flashing.

The top-level `ttos-provision-carNN.conf` files are the same content under
per-car names, kept for reading and diffing. Do not flash those.

## Do not run gen-fleet.py

It mints new random values for everything. `firmware-constants.h` has already been
produced from the current table and its Data IDs and unlock codes are what firmware
gets built against. Re-running silently desynchronises firmware from provisioning,
and a mismatch presents as an unsolvable challenge rather than an error.

It also **will not run on arcana**: it imports `crypt`, removed from the stdlib in
Python 3.13 (PEP 594), and this box is on 3.14. If a genuine from-scratch
regeneration is ever needed, replace `crypt.crypt` with `openssl passwd -6` or
`passlib`.

To re-render the per-car `.conf` files — after a template
change, or if they go missing — use the deterministic renderer instead:

```bash
python3 provisioning/render-fleet.py
```

It reads `fleet-table.csv`, generates nothing, and is safe to run repeatedly.

## Verification performed

- All 8 VINs match the ISO 3779 algorithm in `gen-fleet.py`, and each check digit
  validates against its own VIN. VIN generation is deterministic from car number,
  so these are reproducible without the CSV.
- Data IDs and C2/C3 unlock codes in `firmware-constants.h` match the CSV for all
  8 cars; all 16 Data IDs are in `0x0001`–`0xFFFE`, L≠R per car, no collisions
  across the fleet.
- All 24 unlock codes are 8 chars, correctly challenge-prefixed, and use only the
  no-`0`/`O`/`1`/`I`/`L` alphabet.
- Every `console_hash` verifies against its plaintext in `OPERATOR-SECRETS.md`
  (`openssl passwd -6 -salt <salt>`).
- VIN, ECU serial, PSK, console password and all three code sets are unique per
  car; WiFi channels are distinct and non-DFS.

## Gotcha

`fleet-table.csv` has **CRLF line endings** (Python's `csv` writer default). Parse
it with a real CSV reader, or strip `\r` — a naive `awk -F,` picks up a trailing
carriage return on the last field and every hash comparison fails. The rendered
`.conf` files are clean LF; `ttos-provision.sh` handles CRLF either way.
