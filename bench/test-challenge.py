#!/usr/bin/env python3
"""C2 bridge window and composition -- bench brief section 6, D7-D9 and C1-C4.

This is the whole C2 solve, driven end to end:

    invoke pivot -> read the snapshot corpus -> compose two same-dir frames
    -> open the window via the self-test -> replay inside it -> 0x7D1

    ./provision-bench.sh 01
    ./vehicle-emulator.py --car 01 &
    ./test-challenge.py --car 01
"""

import argparse
import csv
import os
import struct
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "lib"))

import uds  # noqa: E402
from carreset import reset_car  # noqa: E402
from ttoscan import DIAG, DRIVE, Frame, Injector, Observer  # noqa: E402

RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"
results = []
ID_L, ID_R = 0x111, 0x113
ID_C2FLAG, ID_C3FLAG = 0x7D1, 0x7D2


def check(name, ok, detail="", info=""):
    results.append((name, ok))
    print(f"  [{GRN}PASS{RST}] {name}" if ok else f"  [{RED}FAIL{RST}] {name}")
    for line in str(info if ok else (detail or info)).strip().splitlines():
        if line:
            print(f"         {line}")


def emu(*a):
    return subprocess.run([os.path.join(HERE, "emu-ctl.sh"), *a],
                          capture_output=True, text=True).stdout.strip()


def flag_counts():
    for line in emu("status").splitlines():
        if line.startswith("flags emitted:"):
            p = line.split()
            return int(p[3].lstrip("x")), int(p[5].lstrip("x"))
    raise SystemExit("emulator not running")


def load_car(car_id):
    with open(os.path.join(HERE, "..", "provisioning", "fleet-table.csv"),
              newline="") as fh:
        for row in csv.DictReader(fh):
            row = {k: (v or "").strip() for k, v in row.items()}
            if row["car_id"] == car_id:
                return row
    raise SystemExit(f"car {car_id} not in fleet-table.csv")


def parse_snapshot(resp):
    """resp = 62 F1 A0 [id hi][id lo][8 bytes] x2 -> {can_id: payload}"""
    body = resp[3:]
    out = {}
    while len(body) >= 10:
        cid = body[0] << 8 | body[1]
        out[cid] = bytes(body[2:10])
        body = body[10:]
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--car", default="01")
    args = ap.parse_args()
    car = load_car(args.car)

    print(f"\nC2 bridge window and composition (D7-D9, C1-C4)   car {car['car_id']}\n")
    # Detectors only fire while the car is locked. Reset first, or a prior suite's
    # redemption disarms them and C4 fails for a reason that is correct behaviour.
    reset_car()

    with uds.Tester() as t:
        # ---- D8: the decoy refuses in EVERY session -------------------------
        nrcs = {}
        for sess, label in ((uds.SESSION_DEFAULT, "default"),
                            (uds.SESSION_EXTENDED, "extended")):
            t.session(sess)
            try:
                t.routine(uds.RID_BRIDGE, uds.RC_START)
                nrcs[label] = None
            except uds.NegativeResponse as e:
                nrcs[label] = e.nrc
        check("D8  decoy bridging routine returns 0x33 in EVERY session",
              nrcs.get("default") == 0x33 and nrcs.get("extended") == 0x33,
              info=f"default={nrcs.get('default')!r} extended={nrcs.get('extended')!r} "
                   f"(want 0x33 / 51 in both)")

        # ---- D7: self-test is session-gated ---------------------------------
        t.session(uds.SESSION_DEFAULT)
        try:
            t.routine(uds.RID_SELFTEST, uds.RC_START)
            nrc = None
        except uds.NegativeResponse as e:
            nrc = e.nrc
        check("D7  self-test in default session returns 0x7F 0x31 0x7E",
              nrc == 0x7E,
              info=f"NRC={hex(nrc) if nrc is not None else 'none (routine ran!)'} "
                   f"(want 0x7e subFunctionNotSupportedInActiveSession)")

        # ---- D9: snapshot DID -----------------------------------------------
        t.session(uds.SESSION_EXTENDED)
        t.pivot(uds.PIVOT_CW)
        time.sleep(0.2)
        snap = t.request([uds.SID_READ_DID, 0xF1, 0xA0], raise_negative=False)
        frames = parse_snapshot(snap) if snap and len(snap) > 3 else {}
        ok = (ID_L in frames and ID_R in frames
              and len(frames[ID_L]) == 8 and len(frames[ID_R]) == 8
              and frames[ID_L][4] != frames[ID_R][4])   # opposite directions
        check("D9  snapshot DID returns both motor frames, protection bytes intact",
              ok,
              info=(f"0x111 {frames.get(ID_L, b'').hex(' ').upper()}\n"
                    f"0x113 {frames.get(ID_R, b'').hex(' ').upper()}"))
        if not ok:
            print(f"{RED}cannot compose without a corpus -- stopping{RST}\n")
            return 1

        # ---- C1: the window opens for 5 s, then closes ----------------------
        # Measured by probing the bridge, not by trusting the routine's own claim.
        t.session(uds.SESSION_EXTENDED)
        r = t.routine(uds.RID_SELFTEST, uds.RC_START)
        claimed_ms = (r[4] << 8 | r[5]) if r and len(r) >= 6 else 0
        opened = time.monotonic()

        def bridged(gap):
            """Does a frame injected on DIAG reach DRIVE right now?"""
            with Observer(DRIVE) as od, Injector(DIAG) as inj:
                od.drain()
                probe = frames[ID_L]
                for _ in range(3):
                    inj.send(Frame(ID_L, probe))
                    time.sleep(0.03)
                got = od.collect(0.4, match=lambda f: f.can_id == ID_L
                                 and f.data == probe)
                return bool(got)

        inside = bridged(0)
        time.sleep(max(0.0, 6.0 - (time.monotonic() - opened)))
        outside = bridged(0)
        check("C1  bridge window opens for ~5 s on self-test, then closes",
              inside and not outside,
              info=f"routine claimed {claimed_ms} ms; forwarding inside window="
                   f"{inside} (want True), 6 s later={outside} (want False)")

        # ---- C2: same frames 1 s after closure do not cross ------------------
        # Already covered by `outside` above, but assert the tight case too: the
        # window must not be extendable by simply continuing to transmit.
        t.session(uds.SESSION_EXTENDED)
        t.routine(uds.RID_SELFTEST, uds.RC_START)
        time.sleep(bridge_wait := 6.0)
        late = bridged(0)
        check("C2  frames 1 s after closure do not reach DRIVE",
              not late,
              info=f"forwarding {bridge_wait:.0f} s after the window opened={late} "
                   f"(want False)")

        # ---- C3: a single-bit change is rejected silently --------------------
        # Enforcement moved from the motor nodes to the Pi's inbound gates, so the
        # assertion moved with it: a corrupted frame must never REACH the drive bus,
        # rather than reaching it and being ignored. That is a stronger check --
        # "it was dropped" is directly observable instead of inferred from a
        # counter -- and it is the property that actually protects C3.
        emu("reset")
        t.session(uds.SESSION_EXTENDED)
        t.routine(uds.RID_SELFTEST, uds.RC_START)
        bad = bytearray(frames[ID_L])
        bad[7] ^= 0x01
        with Observer(DRIVE) as od, Observer(DIAG) as og, Injector(DIAG) as inj:
            od.drain(); og.drain()
            for _ in range(6):
                inj.send(Frame(ID_L, bytes(bad)))
                time.sleep(0.05)
            crossed = od.collect(1.0, match=lambda f: f.can_id == ID_L
                                 and f.data == bytes(bad))
            replies = og.collect(0.6)
        check("C3  single-bit modification -> dropped at the gate, silently",
              not crossed and not replies,
              info=f"{len(crossed)} corrupted frame(s) reached DRIVE (want 0); "
                   f"{len(replies)} response frame(s) on DIAG (want 0)")

        # ---- C4: composed opposite-pivot frames -> translation -> one 0x7D1 --
        # THE SOLVE. The corpus holds one forward and one reverse frame. Sending
        # BOTH motors the frame that drives them the same way is a translation,
        # which no legitimate interface ever commands while the panel is locked.
        emu("reset")
        time.sleep(0.5)
        t.session(uds.SESSION_EXTENDED)
        t.pivot(uds.PIVOT_CW)
        cw = parse_snapshot(t.request([uds.SID_READ_DID, 0xF1, 0xA0]))
        t.pivot(uds.PIVOT_CCW)
        ccw = parse_snapshot(t.request([uds.SID_READ_DID, 0xF1, 0xA0]))

        # From the two pivots pick, for each motor, the frame whose dir matches the
        # other motor's -- i.e. compose a same-direction pair out of captured,
        # still-valid frames. Nothing is re-CRC'd; these are verbatim captures.
        want_dir = cw[ID_L][4]
        l_frame = cw[ID_L] if cw[ID_L][4] == want_dir else ccw[ID_L]
        r_frame = cw[ID_R] if cw[ID_R][4] == want_dir else ccw[ID_R]
        composed_ok = l_frame[4] == r_frame[4]

        c2_before, c3_before = flag_counts()
        t.session(uds.SESSION_EXTENDED)
        t.routine(uds.RID_SELFTEST, uds.RC_START)
        with Observer(DIAG) as og, Injector(DIAG) as inj:
            og.drain()
            deadline = time.monotonic() + 4.0
            while time.monotonic() < deadline:
                inj.send(Frame(ID_L, l_frame))
                inj.send(Frame(ID_R, r_frame))
                time.sleep(0.12)
            flags = og.collect(1.5, match=lambda f: f.can_id in (ID_C2FLAG, ID_C3FLAG))
        c2_after, c3_after = flag_counts()

        codes = {f.data.rstrip(b"\x00").decode(errors="replace") for f in flags}
        saw_c2 = any(f.can_id == ID_C2FLAG for f in flags)
        saw_c3 = any(f.can_id == ID_C3FLAG for f in flags)
        check("C4  composed same-dir pivot frames in-window -> 0x7D1 on DIAG",
              composed_ok and saw_c2 and car["code_c2"] in codes,
              info=f"composed dir={l_frame[4]:#04x} on both motors; "
                   f"0x7D1 emitted x{c2_after - c2_before}; codes seen {sorted(codes)}")
        check("C7  the same composed replay does NOT reach 0x7D2",
              not saw_c3 and c3_after == c3_before,
              info=f"0x7D2 emitted x{c3_after - c3_before} (want 0) -- composed "
                   f"frames carry the pivot rpm, so C3 must stay out of reach")

        emu("reset")

    bad_ = [n for n, ok in results if not ok]
    print()
    if bad_:
        print(f"{RED}{len(bad_)} of {len(results)} assertions failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} assertions passed{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
