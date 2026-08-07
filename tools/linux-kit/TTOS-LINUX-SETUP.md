# TTOS CTF — Linux operator setup

**For:** a Claude Code session on a Linux laptop that needs to drive a TTOS CTF car
with a **PEAK PCAN-USB (Pro) FD** adapter.

**Why Linux:** the same PEAK adapter works two ways. On macOS it goes through
MacCAN/libPCBUSB, which on this hardware transmitted fine but silently dropped every
CAN FD *reply* — commands landed, answers vanished. On Linux the adapter binds the
in-kernel `peak_usb` driver via **socketcan**, which is the exact path the bench host
already uses successfully. No MacCAN, no libPCBUSB, no FD-receive problem.

This kit is three files, meant to sit in one folder:

```
ttos-can-up.sh        # brings the adapter up and finds the diagnostic bus
ttos-ctf-tool.py      # the CTF tool (pure Python stdlib — nothing to pip install)
TTOS-LINUX-SETUP.md   # this file
```

---

## TL;DR

```sh
cd <this folder>
./ttos-can-up.sh
```

It re-runs itself under `sudo`, brings up every CAN interface at 500 k/1 M CAN FD,
identifies which one is the car's diagnostic bus, and prints the exact `walk` command
to run. If it prints a `READY` block, you are done — copy the command it gives you.

---

## What the adapter and the bus are

- **Adapter:** PEAK PCAN-USB FD, possibly the dual-channel **Pro FD**. A dual adapter
  shows up as **two** interfaces (`can0` and `can1`); a single-channel one as a single
  interface. The script handles either — you do not need to know which is which.
- **Bus:** the car's **diagnostic** tap. **500 kbit** arbitration, **1 Mbit** data,
  **CAN FD**, ISO. FD is not optional: every reply in this game (VIN, the pivot flag,
  the snapshot) is longer than 8 bytes, so a classic-only setup would send fine and
  never receive an answer.
- You plug **one** connector into the car's diagnostic port. On a dual adapter the
  other channel is unused; the script will see it answer nothing and ignore it.

---

## Prerequisites (Claude Code: check and install these)

1. **A recent-ish kernel with `peak_usb`.** It is an in-tree module; nothing to
   install on any mainstream distro from the last decade. Confirm after plugging in:
   ```sh
   dmesg | grep -i peak        # should show the adapter attaching
   ip -o link show type can    # should list can0 (and can1 on a dual adapter)
   ```
2. **python3** — the tool is stdlib-only (raw `AF_CAN` socket). No `python-can`, no
   pip. `apt install python3` if somehow absent.
3. **can-utils** (recommended, not required) for `candump`/`cansend` when hand-debugging:
   ```sh
   sudo apt install can-utils
   ```
4. **Root.** Raw CAN sockets and `ip link` need it. The script re-execs under `sudo`;
   run the tool by hand with `sudo` too.

---

## Manual path (if you would rather not run the script)

```sh
# 1. driver + see the interface
sudo modprobe peak_usb
ip -o link show type can

# 2. bring it up as CAN FD, 500k/1M  (repeat for can1 if the adapter is dual-channel)
#    This is the exact command the bench host uses; let the kernel pick sample points.
sudo ip link set can0 down
sudo ip link set can0 up type can bitrate 500000 dbitrate 1000000 fd on

# 3. which interface is the diagnostic bus?  It answers a VIN; the drive bus does not.
#    (vin sends no motion command — safe to try on each.)
sudo python3 ttos-ctf-tool.py --transport socketcan --iface can0 vin
sudo python3 ttos-ctf-tool.py --transport socketcan --iface can1 vin   # dual adapter only
```

The interface that prints a VIN like `TTKTREK17TC000003` is your diagnostic bus.

---

## Running the game

Replace `can0` with whatever the script (or the VIN test) identified.

```sh
# The whole chain, C1 -> C2 -> C3, with the flags printed at each step:
sudo python3 ttos-ctf-tool.py \
     --transport socketcan --iface can0 \
     --salt dd1b35d820f614e9c94f6a0e4f34cbc3 walk

# Single steps:
sudo python3 ttos-ctf-tool.py --transport socketcan --iface can0 vin
sudo python3 ttos-ctf-tool.py --transport socketcan --iface can0 pivot     # car turns in place
sudo python3 ttos-ctf-tool.py --transport socketcan --iface can0 snapshot
sudo python3 ttos-ctf-tool.py --transport socketcan --iface can0 monitor   # passive listen, ^C to stop
```

`--salt` is a **global** option: it goes **before** the `walk` subcommand, not after.
The fleet salt is `dd1b35d820f614e9c94f6a0e4f34cbc3` (it is not a secret — it also
ships in the panel's page source as `deriveServiceKey`).

Add `--debug` to any command to print every frame in and out.

---

## What "working" looks like

`walk` should reach `full chain OK` and print three flags:

```
FLAG{1D8YZGBT}     (C1, from the pivot routine)
FLAG{2FQYWXDM}     (C2, seen on the bus as the car is caught translating)
FLAG{3CX5E77Z}     (C3, after forging a drive command)
```

(Those are the shared fleet codes. Every car emits the same three.)

---

## Troubleshooting

- **`No CAN interfaces present`** — `peak_usb` did not bind. Re-plug, `sudo modprobe
  peak_usb`, check `dmesg | grep -i peak`. A USB hub or a bad cable is the usual cause.
- **`No interface answered a VIN`** — the request is going out but nothing replies.
  On Linux this is almost never the adapter (it is the proven path). Check, in order:
  the connector is on the **diagnostic** port, CAN-H/CAN-L are not swapped, the car is
  powered, and `ttos-dashboard` is running on it. A **silent** DIAG bus on `candump`
  is normal — it only speaks when spoken to.
- **The car pivots but you still get no VIN** — that was the macOS failure mode
  (transmit works, FD receive dies). It should NOT happen on Linux/socketcan. If it
  does, run `... vin --debug`: an `RX 7E8#...` line means the reply arrived; no `RX`
  line means FD frames are not being delivered, which on Linux would point at a
  genuinely old kernel — try another machine.
- **`Permission denied` / nothing happens** — you forgot `sudo`. Raw CAN needs root.

---

## For the panel (separate from the CAN tool)

The tool above talks to the car over CAN and needs no WiFi. To reach the **panel**
(to paste flags, watch buses), join the car's AP in a browser:

```
SSID   ttos-car-NN         (per car, on the placard)
PSK    TINYTREKCTF         (shared across the fleet)
URL    http://192.168.244.1
Browser: Safari or Firefox — Chrome force-upgrades :80 to HTTPS and the panel looks dead.
```
