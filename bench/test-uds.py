#!/usr/bin/env python3
"""Diagnostic server -- bench brief section 6, D1 through D10.

The bench must be staged as the same car the emulator is running:

    ./provision-bench.sh 01
    ./vehicle-emulator.py --car 01 &
    ./test-uds.py --car 01
"""

import argparse
import csv
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "lib"))

import uds  # noqa: E402
from ttoscan import DRIVE, Observer, crc8_j1850  # noqa: E402
import struct  # noqa: E402

RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"
results = []

ID_L, ID_R = 0x111, 0x113
PIVOT_STEPS = 102


def check(name, ok, detail="", info=""):
    """detail is shown only on FAILURE, info always.

    Keeping them separate matters: a failure message printed under a PASS reads as
    a contradiction, and D3b's "protection bytes do not match" appearing above a
    green PASS is exactly the kind of thing that gets a correct run investigated.
    """
    results.append((name, ok))
    print(f"  [{GRN}PASS{RST}] {name}" if ok else f"  [{RED}FAIL{RST}] {name}")
    for line in str(info if ok else (detail or info)).strip().splitlines():
        if line:
            print(f"         {line}")


def skip(name, why):
    print(f"  [{YLW}SKIP{RST}] {name}")
    print(f"         {why}")


def load_car(car_id):
    with open(os.path.join(HERE, "..", "provisioning", "fleet-table.csv"),
              newline="") as fh:
        for row in csv.DictReader(fh):
            row = {k: (v or "").strip() for k, v in row.items()}
            if row["car_id"] == car_id:
                return row
    raise SystemExit(f"car {car_id} not in fleet-table.csv")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--car", default="01")
    args = ap.parse_args()
    car = load_car(args.car)
    did_l, did_r = int(car["dataid_l"], 16), int(car["dataid_r"], 16)

    print(f"\ndiagnostic server (D1-D10)   car {car['car_id']}\n")

    with uds.Tester() as t:
        # ---- D1: the bus is silent with nothing outstanding -----------------
        # A diagnostic port that chatters unprompted is both unrealistic and a
        # free corpus. The server only ever transmits in response to a request.
        t.obs.drain()
        stray = t.obs.collect(2.0)
        check("D1  DIAG bus is silent with no request outstanding", not stray,
              f"{len(stray)} unsolicited frame(s), e.g. {stray[0] if stray else ''}")

        # ---- D2: functional addressing works --------------------------------
        r = t.request([uds.SID_SESSION, uds.SESSION_DEFAULT], functional=True,
                      raise_negative=False)
        check("D2  functional request 0x7DF gets a response from 0x7E8",
              bool(r) and r[0] == uds.SID_SESSION + uds.RESP_OFFSET,
              info=f"got {r.hex(' ') if r else 'no response'}")

        # ---- D5: VIN readable in the default session ------------------------
        t.session(uds.SESSION_DEFAULT)
        r = t.read_did(uds.DID_VIN, raise_negative=False)
        vin = r[3:].decode(errors="replace") if r and len(r) > 3 else ""
        check("D5  0xF190 (VIN) readable in the default session",
              vin == car["vin"], info=f"got {vin!r}, expected {car['vin']!r}")

        # ---- D6: ECU serial gated behind the extended session ---------------
        try:
            t.read_did(uds.DID_ECU_SERIAL)
            gated = False
            nrc = None
        except uds.NegativeResponse as e:
            gated, nrc = True, e.nrc
        t.session(uds.SESSION_EXTENDED)
        r = t.read_did(uds.DID_ECU_SERIAL, raise_negative=False)
        serial = r[3:].decode(errors="replace") if r and len(r) > 3 else ""
        check("D6  0xF18C refused in default, readable in extended",
              gated and serial == car["ecu_serial"],
              info=f"default-session NRC={nrc if nrc is None else hex(nrc)} "
                   f"(want 0x7f), extended read={serial!r}")

        # ---- D10: S3 timeout ------------------------------------------------
        # Expiry first: an extended session must fall back to default after 5 s of
        # silence. Then the same wait WITH TesterPresent must survive -- without
        # that second half, a server that never opened the session at all passes.
        t.session(uds.SESSION_EXTENDED)
        time.sleep(6.5)
        try:
            t.read_did(uds.DID_ECU_SERIAL)
            expired = False
        except uds.NegativeResponse:
            expired = True

        t.session(uds.SESSION_EXTENDED)
        for _ in range(4):
            time.sleep(1.6)
            t.tester_present()
        try:
            r = t.read_did(uds.DID_ECU_SERIAL)
            survived = bool(r)
        except uds.NegativeResponse:
            survived = False
        check("D10 session expires after S3 (5 s); survives with TesterPresent",
              expired and survived,
              info=f"expired-when-idle={expired} (want True), "
                   f"survived-with-3E={survived} (want True)")

        # ---- D3 / D4: the pivot routine -------------------------------------
        # Watch the DRIVE bus while the routine runs. These frames come from the
        # DUT, so they are genuine wire traffic and a wire-only Observer sees them.
        for label, direction, want_l, want_r in (
                ("clockwise", uds.PIVOT_CW, 0x01, 0x02),
                ("counter-clockwise", uds.PIVOT_CCW, 0x02, 0x01)):
            with Observer(DRIVE) as od:
                od.drain()
                try:
                    code = t.pivot(direction)
                    err = None
                except uds.NegativeResponse as e:
                    code, err = None, e
                cmds = od.collect(1.0, match=lambda f: f.can_id in (ID_L, ID_R))

            if err:
                check(f"D3  pivot {label}", False, str(err))
                check(f"D4  pivot {label} response carries the C1 flag", False, str(err))
                continue

            lf = [f for f in cmds if f.can_id == ID_L]
            rf = [f for f in cmds if f.can_id == ID_R]
            ok_count = len(lf) == 1 and len(rf) == 1
            ok_shape = False
            detail = f"{len(lf)} left command(s), {len(rf)} right command(s)"
            if ok_count:
                l, r_ = lf[0].data, rf[0].data
                steps_l = struct.unpack(">I", l[:4])[0]
                steps_r = struct.unpack(">I", r_[:4])[0]
                # Rotating in place means OPPOSITE directions, equal step counts.
                ok_shape = (steps_l == steps_r == PIVOT_STEPS
                            and l[4] == want_l and r_[4] == want_r)
                detail = (f"L: steps={steps_l} dir={l[4]:#04x} rpm={l[5]} len={len(l)}\n"
                          f"R: steps={steps_r} dir={r_[4]:#04x} rpm={r_[5]} len={len(r_)}\n"
                          f"want steps={PIVOT_STEPS} dirL={want_l:#04x} dirR={want_r:#04x}")
            check(f"D3  pivot {label}: in place, {PIVOT_STEPS} steps, one command per wheel",
                  ok_count and ok_shape, info=detail)

            # The protection bytes must validate against THIS car's Data IDs, or
            # the Pi and the motor firmware disagree and Phase 3 rejects everything
            # in silence.
            if ok_count and len(lf[0].data) == 8:
                crc_ok = all(
                    crc8_j1850(struct.pack("<H", did) + f.data[:7]) == f.data[7]
                    for f, did in ((lf[0], did_l), (rf[0], did_r)))
                check(f"D3b pivot {label}: CRC validates against car {car['car_id']}'s Data IDs",
                      crc_ok, "protection bytes do not match -- the Pi and the "
                              "motor firmware would disagree in Phase 3")

            got = code.decode(errors="replace") if code else ""
            check(f"D4  pivot {label} response carries the C1 flag",
                  got == car["code_c1"],
                  info=f"got {got!r}, expected {car['code_c1']!r}")

        # ---- Phase 3 items --------------------------------------------------
        skip("D7  self-test routine in default session returns 0x7F 0x31 0x7E",
             "self-test routine is Phase 3 (bridge window)")
        skip("D8  decoy bridging routine returns 0x33 in every session",
             "decoy routine is Phase 3")
        skip("D9  snapshot DID returns both motor frames with protection intact",
             "snapshot DID is Phase 3")
        skip("D11 server tolerates ENOBUFS indefinitely with DIAG down",
             "needs a multi-hour soak; run separately")

    bad = [n for n, ok in results if not ok]
    print()
    if bad:
        print(f"{RED}{len(bad)} of {len(results)} assertions failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} assertions passed{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
