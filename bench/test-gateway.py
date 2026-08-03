#!/usr/bin/env python3
"""Gateway isolation -- bench brief section 6, G1 through G5.

Every one of these is a claim about which WIRE a frame did or did not appear on,
so each observes the FAR bus. Cross-bus observation is immune to the loopback
trap (the interfaces differ), but the observers are wire-only regardless.

Requires: bench-up.sh passed, the DUT reachable, vehicle-emulator.py running
(G2 and G5 need a node transmitting on DRIVE).

    ./test-gateway.py
"""

import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "lib"))

from ttoscan import DIAG, DRIVE, Frame, Injector, Observer  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"
results = []

ID_POWER, ID_L, ID_R = 0x115, 0x111, 0x113
ID_HB, ID_BEACON = 0x100, 0x116
ID_C2, ID_C3 = 0x7D1, 0x7D2

# How long to watch before concluding "never arrived". The gateway forwards in
# kernel context, so a leak would show up in microseconds -- but a window this
# short would also pass if the bus were dead, which is why every negative case
# below is paired with a liveness check.
WINDOW = 1.5


def emu(*a):
    return subprocess.run([os.path.join(HERE, "emu-ctl.sh"), *a],
                          capture_output=True, text=True).stdout.strip()


def check(name, ok, detail=""):
    results.append((name, ok))
    print(f"  [{GRN}PASS{RST}] {name}" if ok else f"  [{RED}FAIL{RST}] {name}")
    if detail and not ok:
        for line in detail.strip().splitlines():
            print(f"         {line}")


def bus_is_live(obs, label):
    """A negative assertion over a dead bus is worthless. Confirm SOMETHING is
    arriving before believing 'nothing arrived'."""
    got = obs.collect(1.5)
    if not got:
        print(f"  {YLW}!! {label} carried no traffic at all during the check -- "
              f"the negative results below are unproven{RST}")
        return False
    return True


def main():
    print("\ngateway isolation (G1-G5)\n")

    # ---- G1: 0x115 injected on DIAG must never reach DRIVE ------------------
    with Observer(DRIVE) as od, Injector(DIAG) as inj:
        od.drain()
        live = bus_is_live(od, "DRIVE")
        od.drain()
        for _ in range(10):
            inj.send(Frame(ID_POWER, bytes([0x01])))
            time.sleep(0.05)
        leaked = od.collect(WINDOW, match=lambda f: f.can_id == ID_POWER)
    check("G1  0x115 (BMS power) on DIAG does not reach DRIVE",
          not leaked and live,
          f"{len(leaked)} frame(s) crossed inbound -- a contestant could cut the "
          f"12V rail from the diagnostic tap.")

    # ---- G4: drive commands injected on DIAG must never reach DRIVE ---------
    with Observer(DRIVE) as od, Injector(DIAG) as inj:
        od.drain()
        for _ in range(10):
            inj.send(Frame(ID_L, bytes([0, 0, 0, 100, 1, 100])))
            inj.send(Frame(ID_R, bytes([0, 0, 0, 100, 1, 100])))
            time.sleep(0.05)
        leaked = od.collect(WINDOW, match=lambda f: f.can_id in (ID_L, ID_R))
    check("G4  0x111/0x113 on DIAG do not reach DRIVE (no bridge window open)",
          not leaked,
          f"{len(leaked)} drive command(s) crossed inbound with no window open.")

    # ---- G3: an FD frame on DIAG must never reach DRIVE ---------------------
    # DRIVE is configured classic on purpose, so a genuine FD frame arriving there
    # would present as an ERROR frame rather than as data -- errors=True is what
    # makes this observable at all.
    with Observer(DRIVE, errors=True) as od, Injector(DIAG) as inj:
        od.drain()
        try:
            for _ in range(5):
                inj.send(Frame(0x555, bytes(range(32)), fd=True, brs=True))
                time.sleep(0.05)
            sent_fd = True
        except OSError as e:
            sent_fd = False
            print(f"  {YLW}!! could not transmit an FD frame on DIAG: {e}{RST}")
        # Match only what would constitute a leak. The drive bus legitimately
        # carries the DUT's 0x100 heartbeat throughout, and a bare collect() counts
        # that as evidence of a gateway failure -- which it did on first run.
        leaked = od.collect(WINDOW,
                            match=lambda f: f.err or f.fd or f.can_id == 0x555)
    check("G3  an FD frame on DIAG does not reach DRIVE (no data, no error frames)",
          sent_fd and not leaked,
          f"{len(leaked)} frame(s)/error(s) on DRIVE: {leaked[:3]}\n"
          "An FD frame on the drive bus is a form error to an MCP2515 motor node:\n"
          "error-passive, then bus-off, then propulsion stops.")

    # ---- G2: drive-bus traffic must never appear on DIAG --------------------
    # The emulated BMS beacons 0x116 at 5 Hz and the DUT heartbeats 0x100 at 5 Hz,
    # so DRIVE is busy without anything needing to be injected.
    drive_ids = (ID_HB, ID_BEACON, ID_L, ID_R)
    with Observer(DIAG) as og:
        og.drain()
        leaked = og.collect(2.5, match=lambda f: f.can_id in drive_ids)
    check("G2  drive traffic (0x100/0x116/0x111/0x113) does not appear on DIAG",
          not leaked,
          f"{len(leaked)} frame(s) leaked outbound, e.g. {repr(leaked[0]) if leaked else ''}\n"
          "Forwarding drive traffic hands contestants the C2 corpus by passive\n"
          "sniffing, which collapses the challenge the gateway exists to protect.")

    # ---- G5: flag frames MUST reach DIAG ------------------------------------
    # "Omitting this forwarding rule is the easiest way to ship an unsolvable event."
    emu("reset")
    time.sleep(0.5)
    with Observer(DIAG) as og, Injector(DRIVE) as inj:
        og.drain()
        for _ in range(25):          # same-dir at rpm=100 -> trips C2 and C3
            for cid in (ID_L, ID_R):
                inj.send(Frame(cid, bytes([0, 0, 0, 100, 0x01, 100])))
            time.sleep(0.08)
        got = og.collect(2.0, match=lambda f: f.can_id in (ID_C2, ID_C3))
    ids = {f.can_id for f in got}
    check("G5  flag frames 0x7D1 and 0x7D2 DO reach DIAG",
          ID_C2 in ids and ID_C3 in ids,
          f"saw {[hex(i) for i in sorted(ids)]} -- missing "
          f"{[hex(i) for i in (ID_C2, ID_C3) if i not in ids]}.\n"
          "Without this rule the codes never reach the contestant and the\n"
          "challenge is unsolvable. Is the emulator running?")
    if got:
        print(f"         codes seen: "
              f"{sorted({f.data.rstrip(bytes(1)).decode(errors='replace') for f in got})}")
    emu("reset")

    bad = [n for n, ok in results if not ok]
    print()
    if bad:
        print(f"{RED}{len(bad)} of {len(results)} gateway assertions failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} gateway assertions passed{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
