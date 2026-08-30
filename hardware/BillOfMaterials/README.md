# Bill of Materials

Everything you need to source to build one Tiny Trek, split across one overview
list and three per-board component lists.

The **CSV files are the source of truth.** [`BOM.xlsx`](BOM.xlsx) is the same data
in one workbook (one sheet per CSV) for people who would rather work in a
spreadsheet — if the two ever disagree, trust the CSVs.

## The files

| File | What it covers |
|---|---|
| [BOM-Overview.csv](BOM-Overview.csv) | The whole-vehicle list: printed parts, sourced parts (Pi, motors, wheels, battery), wiring, fasteners, and recommended tools. Start here. |
| [BOM-MotorControlBoard.csv](BOM-MotorControlBoard.csv) | Components for one Motor Control Board. **You need two per car.** |
| [BOM-PowerDistroBoard.csv](BOM-PowerDistroBoard.csv) | Components for the Power Distribution Board. One per car. |
| [BOM-UltrasonicSensorBoard.csv](BOM-UltrasonicSensorBoard.csv) | Components for the Ultrasonic Sensor Board. Optional — one per car if you fit it. |

The per-board lists correspond to the fabrication files in
[`../BoardFiles/`](../BoardFiles/) and the schematics in
[`../SchematicsAndWiring/`](../SchematicsAndWiring/).

## How to read a per-board list

- **Reference Designators** match the silkscreen on the board and the schematic.
  Designators in parentheses — e.g. `(A1)`, `(JP1)` — are the header sockets that
  the module named above plugs into, not parts with their own silkscreen mark.
- **Board** (the first row, no designator) is the bare PCB itself. Order it from
  JLCPCB or any fab house using the Gerbers in [`../BoardFiles/`](../BoardFiles/).
- Where a **Digikey** or **Mouser** part number is given, either distributor
  carries an equivalent; the generic 0805 passives (marked "Generic") are
  whatever you have in your parts bin at the listed value.

## Notes

- **Optional parts** are marked `OPTIONAL` in the overview. The camera, the
  ultrasonic sensor board, the CAN-debug snap connectors, and the ferrule/crimp
  tooling can all be skipped for a first build.
- **Quantities** in the overview assume one complete car. The overview already
  accounts for two Motor Control Boards.
- **Amazon links are affiliate links.** Any equivalent part works; nothing here
  requires buying from a specific vendor.
