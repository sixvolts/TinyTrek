#!/usr/bin/env python3
"""Measure what the drive-command stream actually looks like on the wire.

Three questions, in the order they should be ruled out:

  1. Is the DRIVE bus congested?          bus load + error/arbitration counters
  2. Do commands arrive regularly?         inter-arrival distribution of 0x111/0x113
  3. Does the motor's step buffer starve?  occupancy sampled from the emulator

Driven from the DUT itself, over loopback to the AP address. That deliberately
removes WiFi AND the browser from the path, so this run is the FLOOR -- the best
the chain can do. If the stream is ragged even here, the fault is in the Pi or the
bus. If it is clean here, whatever the car does worse is contributed by the browser
timer or the wireless link, neither of which is on this path.

    ./measure-drive.py --car 01 --seconds 12
"""

import argparse
import csv
import json
import os
import statistics
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "lib"))

from ttoscan import DRIVE, Observer  # noqa: E402

DUT = os.environ.get("DUT", "192.168.4.133")
DUT_USER = os.environ.get("DUT_USER", "ttos")
SSH = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", f"{DUT_USER}@{DUT}"]
BASE = "http://192.168.244.1"

ID_L, ID_R = 0x111, 0x113
BOLD, RED, GRN, YLW, RST = "\033[1m", "\033[31m", "\033[32m", "\033[33m", "\033[0m"


def sh(cmd, **kw):
    return subprocess.run(SSH + [cmd], capture_output=True, text=True, **kw).stdout


def load_car(car_id):
    with open(os.path.join(HERE, "..", "provisioning", "fleet-table.csv"), newline="") as fh:
        for row in csv.DictReader(fh):
            row = {k: (v or "").strip() for k, v in row.items()}
            if row["car_id"] == car_id:
                return row
    raise SystemExit(f"car {car_id} not in fleet-table.csv")


def can_stats(iface="candrive"):
    """Error and arbitration counters straight from the DUT's DRIVE controller.

    THE INTERFACE NAME MATTERS AND HAS BEEN WRONG HERE. This defaulted to "can1"
    until 2026-08-04. That was wrong twice over: once the bus roles were corrected
    can1 was the DIAG controller, so question 1 above was answered with counters
    from the bus it is not about; and after the image moved to role names can1
    stopped existing entirely. sh() returns stdout only, so `ip`'s "device not
    found" goes to stderr, `vals` comes back empty, every delta computes as 0, and
    the report prints a clean bill of health for an interface it never read.

    Hence the raise: no counters is an ERROR, not zero counters. A congestion
    measurement that cannot see the controller must not be able to exonerate it.
    """
    out = sh(f"PATH=/sbin:/usr/sbin:$PATH ip -s -d link show {iface}")
    if not out.strip():
        raise SystemExit(
            f"cannot read counters for {iface} on the DUT -- no such interface?\n"
            f"  ip -br link show {iface}   (role names come from 10-ttos-can.rules;\n"
            f"  an image predating them names these can0/can1 by SPI probe order)")
    vals = {}
    lines = out.splitlines()
    for i, l in enumerate(lines):
        if "re-started" in l and "bus-errors" in l:
            nums = lines[i + 1].split()
            for k, v in zip(["restarts", "bus_errors", "arb_lost", "err_warn",
                             "err_pass", "bus_off"], nums):
                vals[k] = int(v)
    for i, l in enumerate(lines):
        if l.strip().startswith("TX:") and i + 1 < len(lines):
            n = lines[i + 1].split()
            if len(n) >= 4:
                vals["tx_packets"], vals["tx_errors"], vals["tx_dropped"] = \
                    int(n[1]), int(n[2]), int(n[3])
    if "bus_errors" not in vals:
        raise SystemExit(
            f"{iface} exists on the DUT but reports no CAN error counters -- is it a\n"
            f"CAN device, and is it UP? `ip -s -d link show {iface}` returned:\n{out}")
    return vals


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--car", default="01")
    ap.add_argument("--seconds", type=float, default=12.0)
    ap.add_argument("--interval", type=float, default=0.150,
                    help="keepalive period the driver aims for (default 150 ms)")
    args = ap.parse_args()
    car = load_car(args.car)

    print(f"\n{BOLD}drive-stream measurement{RST}   car {car['car_id']}   "
          f"{args.seconds:.0f}s @ {args.interval*1000:.0f} ms target\n")

    # Unlock tier 3 -- /api/control gates drive commands behind the C3 code.
    #
    # CONFIRM IT TOOK. This was fire-and-forget, and when the redemption failed the
    # driver loop below ran to completion against an endpoint that refused every
    # request: no drive frames, no error, and a bus-health section that reported a
    # spotless bus. Redemption can fail for reasons that have nothing to do with the
    # code -- /api/flag rate-limits by IP, so running this straight after a suite
    # that submits several codes gets "too many attempts" and tier 0.
    sh("rm -f /tmp/measjar")
    sh(f"curl -s -c /tmp/measjar -b /tmp/measjar -X POST -H 'Content-Type: application/json' "
       f"-d '{{\"code\":\"{car['code_c3']}\"}}' {BASE}/api/flag")
    tier_txt = sh(f"curl -s -b /tmp/measjar {BASE}/api/info")
    try:
        tier = json.loads(tier_txt or "{}").get("tier", 0)
    except json.JSONDecodeError:
        tier = 0
    if tier != 3:
        print(f"{RED}cannot drive: the C3 redemption left the session at tier {tier}, "
              f"not 3.{RST}")
        print("Wait a minute for /api/flag's rate limit to clear and run again.")
        return 1

    before = can_stats()

    # A driver loop ON THE CAR with a fixed period. No browser, no WiFi.
    n = int(args.seconds / args.interval)
    driver = (f"python3 -c \"import subprocess,time\n"
              f"for i in range({n}):\n"
              f"    t=time.monotonic()\n"
              f"    subprocess.run(['curl','-s','-o','/dev/null','-b','/tmp/measjar',"
              f"'-X','POST','-H','Content-Type: application/json',"
              f"'-d','{{\\\"cmd\\\":\\\"forward\\\"}}','{BASE}/api/control'])\n"
              f"    d={args.interval}-(time.monotonic()-t)\n"
              f"    if d>0: time.sleep(d)\"")

    obs = Observer(DRIVE)
    obs.drain()
    frames = []
    stop = threading.Event()

    def collect():
        while not stop.is_set():
            frames.extend(obs.collect(0.2, match=lambda f: f.can_id in (ID_L, ID_R)))

    # Sample the emulated motor's step buffer while it runs.
    occupancy = []

    def sample():
        while not stop.is_set():
            out = subprocess.run([os.path.join(HERE, "emu-ctl.sh"), "status"],
                                 capture_output=True, text=True).stdout
            for line in out.splitlines():
                if line.startswith("L:"):
                    try:
                        occupancy.append(int(line.split("steps=")[1].split()[0]))
                    except (IndexError, ValueError):
                        pass
            time.sleep(0.05)

    tc = threading.Thread(target=collect, daemon=True); tc.start()
    ts = threading.Thread(target=sample, daemon=True); ts.start()
    t0 = time.monotonic()
    sh(driver, timeout=args.seconds + 60)
    elapsed = time.monotonic() - t0
    time.sleep(0.4)
    stop.set(); tc.join(timeout=2); ts.join(timeout=2)
    obs.close()
    after = can_stats()

    # ---------------------------------------------------------------- 1. bus --
    print(f"{BOLD}1. Bus health and load{RST}")
    delta = {k: after.get(k, 0) - before.get(k, 0) for k in after}
    total = len(frames)
    # Classic 11-bit frame with 8 data bytes, worst case with stuffing ~130 bits.
    load = (total * 130) / (500_000 * elapsed) * 100 if elapsed else 0
    print(f"   drive frames seen        : {total} in {elapsed:.1f}s "
          f"({total/elapsed:.1f}/s)")
    print(f"   estimated bus load       : {load:.2f}%  (drive commands only)")
    for k in ("bus_errors", "arb_lost", "err_warn", "err_pass", "bus_off",
              "tx_errors", "tx_dropped"):
        v = delta.get(k, 0)
        mark = f"{RED}" if v else f"{GRN}"
        print(f"   {k:<24} : {mark}{v}{RST}")
    # A CLEAN BUS WITH NO TRAFFIC PROVES NOTHING, and saying otherwise is the exact
    # mistake this script exists to avoid. On 2026-08-04 a run that transmitted zero
    # drive commands -- the session was never raised to tier 3, so /api/control
    # refused every request -- still printed "CONTENTION RULED OUT" in bold. Zero
    # frames means zero errors by construction. The verdict is now gated on there
    # being enough traffic to have stressed anything, and a barren run is a failure
    # of the measurement rather than a finding about the bus.
    if total < 10:
        print(f"   -> {RED}{BOLD}NO VERDICT: only {total} drive frames were transmitted{RST}")
        print("      A quiet bus cannot exonerate itself. Nothing was measured here.")
        # Name the likeliest cause rather than making the operator hunt for it.
        # A PROVISIONED car has TTOS_DASH_DRIVE empty -- ttos-provision sets it that
        # way on purpose, as the read-only safety gate -- and then /api/control
        # accepts the request, replies {"ok":true}, logs "drive disabled", and puts
        # nothing on the wire. There is no signal at the HTTP layer at all.
        dv = sh("sed -n 's/^TTOS_DASH_DRIVE=//p' /etc/default/ttos-dashboard").strip()
        if not dv:
            print(f"      {YLW}TTOS_DASH_DRIVE is EMPTY on this DUT: the dashboard write path")
            print(f"      is disabled, so /api/control replies ok and sends nothing.{RST}")
            print("      That is what ttos-provision does to a competition car. To measure,")
            print("      use a factory-mode DUT, or set TTOS_DASH_DRIVE=candrive and restart")
            print("      ttos-dashboard on a car you are willing to make drivable.")
        else:
            print(f"      TTOS_DASH_DRIVE={dv}, so the write path is live -- the driver loop")
            print("      itself did not reach /api/control. Check the tier and the AP address.")
        print()
        return 1
    verdict = "CONTENTION RULED OUT" if load < 20 and not any(
        delta.get(k, 0) for k in ("bus_errors", "arb_lost", "err_pass", "bus_off")) \
        else "bus shows stress -- investigate"
    print(f"   -> {BOLD}{verdict}{RST}\n")

    # -------------------------------------------------- 2. command regularity --
    print(f"{BOLD}2. Command stream regularity{RST}")
    lf = sorted([f.ts for f in frames if f.can_id == ID_L])
    gaps = [(b - a) * 1000 for a, b in zip(lf, lf[1:])]
    if len(gaps) < 5:
        print(f"   {RED}too few frames to characterise ({len(gaps)} gaps){RST}\n")
    else:
        gaps_s = sorted(gaps)
        p = lambda q: gaps_s[min(len(gaps_s) - 1, int(len(gaps_s) * q))]
        print(f"   target period            : {args.interval*1000:.0f} ms")
        print(f"   mean / median            : {statistics.mean(gaps):.1f} / "
              f"{statistics.median(gaps):.1f} ms")
        print(f"   p95 / p99 / max          : {p(.95):.1f} / {p(.99):.1f} / "
              f"{max(gaps):.1f} ms")
        print(f"   stdev                    : {statistics.pstdev(gaps):.1f} ms")
        # The buffer holds STEPS_MAX=250 steps; at 100 rpm (333 steps/s) that is
        # ~750 ms of cushion. A gap beyond that empties it and the wheel stops.
        starve_ms = 250 / (100 / 60 * 200) * 1000
        bad = [g for g in gaps if g > starve_ms]
        print(f"   gaps > {starve_ms:.0f} ms (buffer would empty): "
              f"{RED if bad else GRN}{len(bad)}{RST}\n")

    # ------------------------------------------------------ 3. spurious stops --
    print(f"{BOLD}3. Buffer-clearing commands in the stream{RST}")
    stops = [f for f in frames if len(f.data) > 4 and f.data[4] == 0x00]
    dirs = {}
    for f in frames:
        if len(f.data) > 4:
            dirs[f.data[4]] = dirs.get(f.data[4], 0) + 1
    print(f"   direction bytes seen     : "
          f"{ {hex(k): v for k, v in sorted(dirs.items())} }")
    print(f"   dir=0x00 (stop/coast)    : {RED if stops else GRN}{len(stops)}{RST}"
          f"  -- each one ZEROES the step buffer instantly")
    rpms = {f.data[5] for f in frames if len(f.data) > 5}
    print(f"   rpm values seen          : {sorted(rpms)}\n")

    # ------------------------------------------------------- 4. buffer starve --
    print(f"{BOLD}4. Emulated motor step buffer{RST}")
    if occupancy:
        zeros = sum(1 for o in occupancy if o == 0)
        print(f"   samples                  : {len(occupancy)}")
        print(f"   min / median / max steps : {min(occupancy)} / "
              f"{int(statistics.median(occupancy))} / {max(occupancy)}")
        print(f"   samples at zero (stalled): {RED if zeros else GRN}{zeros}{RST}"
              f" ({zeros/len(occupancy)*100:.1f}%)")
    else:
        print(f"   {YLW}no samples -- is the emulator running?{RST}")
    print()


if __name__ == "__main__":
    sys.exit(main())
