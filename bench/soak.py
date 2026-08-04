#!/usr/bin/env python3
"""The two long-running assertions, run back to back against a real car.

    ./soak.py --car 02 --iface ttcardiag

Both are "fails quietly" checks. Neither can be shortened without losing what it
tests, which is why they are here rather than in the fast suites.

C5 -- thirty minutes of LEGITIMATE operation must emit ZERO flag frames.
      The detectors are armed the whole time and the car is doing exactly what a
      locked car does: answering diagnostic requests and running its pivot routine.
      A single 0x7D1 or 0x7D2 here means a contestant gets a code for standing
      still, and the failure is invisible in a short test because the C3 detector
      needs 15 qualifying commands inside 3 s to trip -- you only see a false
      positive when something drifts over time.

D11 -- with no tester on the diagnostic bus, the server must sit there for hours
      without crashing, spinning, or flooding the log. This reproduces the state a
      car is in for most of an event: powered, armed, and completely unattended.
      Every transmit onto an unpopulated CAN bus fails ENOBUFS because nothing
      acknowledges it, and code that treats that as an error to retry or report
      will either burn a core or fill the disk. Cannot be tested any other way.
"""

import argparse
import csv
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "lib"))

import uds  # noqa: E402
from ttoscan import Observer  # noqa: E402

RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def car_ssh(host, pw, cmd, timeout=60):
    return subprocess.run(
        ["sshpass", "-p", pw, "ssh", "-o", "StrictHostKeyChecking=no",
         "-o", "BatchMode=no", "-o", "ConnectTimeout=8", f"ttos@{host}", cmd],
        capture_output=True, text=True, timeout=timeout).stdout.strip()


def load_car(car_id):
    with open(os.path.join(HERE, "..", "provisioning", "fleet-table.csv"), newline="") as fh:
        for row in csv.DictReader(fh):
            row = {k: (v or "").strip() for k, v in row.items()}
            if row["car_id"] == car_id:
                return row
    raise SystemExit(f"car {car_id} not in fleet-table.csv")


def operator_password(car_id):
    import re
    p = os.path.join(HERE, "..", "provisioning", "OPERATOR-SECRETS.md")
    for line in open(p):
        m = re.match(rf"\|\s*{car_id}\s*\|\s*`([^`]+)`", line)
        if m:
            return m.group(1)
    raise SystemExit(f"no console password for car {car_id}")


# ---------------------------------------------------------------------- C5 ----

def soak_c5(args, car, host, pw):
    log(f"C5: {args.c5_minutes} minutes of legitimate operation, expecting zero flags")

    # Prove the bus works before committing half an hour to it. A down interface
    # yields a clean-looking "zero flags" result that means nothing at all.
    iface_up(args.iface)
    with uds.Tester(args.iface, timeout=2.0) as probe:
        alive = any(probe.read_did(uds.DID_VIN, raise_negative=False) for _ in range(3))
        if not alive:
            log(f"  C5 ABORT: the car does not answer on {args.iface}")
            return {"name": "C5", "ok": False, "pivots": 0, "flags": 0,
                    "reason": "no response before the soak started"}

    # Detectors only fire while the car is LOCKED. Reset first, or a previously
    # redeemed code stands them down and the whole soak passes for the wrong reason.
    #
    # THE RESET MUST BE VERIFIED. This was a fire-and-forget ssh with its output
    # sent to /dev/null, its return value discarded, and a trailing `; echo done`
    # that guaranteed exit 0 -- so the comment above described a risk the code then
    # did nothing about. Thirty minutes of "zero flags" from a car whose detectors
    # were already stood down is exactly the quiet false pass this file's docstring
    # warns about, and the pre-flight below cannot catch it: a VIN read succeeds
    # regardless of the arm mask.
    #
    # carreset.reset_car() keys on ttos-reset's exit status and aborts loudly.
    sys.path.insert(0, os.path.join(HERE, "lib"))
    os.environ.setdefault("DUT", host)
    os.environ["DUT_PASS"] = pw
    import carreset  # noqa: E402
    carreset.DUT, carreset.DUT_PASS = host, pw
    carreset.reset_car(wait=5.0)

    deadline = time.monotonic() + args.c5_minutes * 60
    pivots = 0
    flags = []
    last_report = 0.0
    direction = uds.PIVOT_CW

    obs = Observer(args.iface)
    obs.drain()
    with uds.Tester(args.iface, timeout=2.0) as t:
        while time.monotonic() < deadline:
            try:
                t.session(uds.SESSION_DEFAULT)
                t.pivot(direction)
                pivots += 1
                direction = (uds.PIVOT_CCW if direction == uds.PIVOT_CW else uds.PIVOT_CW)
            except Exception as e:                       # noqa: BLE001
                log(f"  C5: diagnostic request failed: {e}")
            # Collecting between pivots keeps the observer drained so nothing is
            # missed to a full socket buffer.
            flags += obs.collect(2.0, match=lambda f: f.can_id in (0x7D1, 0x7D2))
            if time.monotonic() - last_report > 300:
                last_report = time.monotonic()
                left = (deadline - time.monotonic()) / 60
                log(f"  C5: {pivots} pivots, {len(flags)} flag frames, {left:.0f} min left")
    obs.close()

    ok = not flags
    log(f"C5 {'PASS' if ok else 'FAIL'}: {pivots} pivot routines, "
        f"{len(flags)} flag frames (want 0)")
    if flags:
        for f in flags[:5]:
            log(f"    LEAKED {f!r}")
    return {"name": "C5", "ok": ok, "pivots": pivots, "flags": len(flags)}


# --------------------------------------------------------------------- D11 ----

def iface_up(iface):
    subprocess.run(["sudo", "ip", "link", "set", iface, "down"], check=False)
    subprocess.run(["sudo", "ip", "link", "set", iface, "up", "type", "can",
                    "bitrate", "500000", "dbitrate", "1000000", "fd", "on"], check=False)
    # A CAN controller needs a moment after coming up before it will carry traffic.
    # Without this the pre-flight below probes a link that is technically UP and not
    # yet usable, and aborts a soak that would have been fine.
    time.sleep(1.5)


def soak_d11(args, car, host, pw):
    log(f"D11: {args.d11_hours} hours with no tester on the diagnostic bus")

    # Take the bench adapter off the bus so the car's diagnostic interface has no
    # peer at all -- exactly the deployed idle state.
    #
    # RESTORED VIA atexit, because an interrupted run otherwise leaves the adapter
    # down and the NEXT run fails "Network is down" on every request -- which reads
    # as a car fault rather than leftover state from a killed test. That happened.
    subprocess.run(["sudo", "ip", "link", "set", args.iface, "down"], check=False)
    log(f"  {args.iface} down; the car's diagnostic bus is now unpopulated")

    def sample():
        raw = car_ssh(host, pw, (
            "P=$(systemctl show ttos-dashboard -p MainPID --value); "
            "S=$(awk '{print $14+$15}' /proc/$P/stat 2>/dev/null); "
            "R=$(awk '/VmRSS/{print $2}' /proc/$P/status 2>/dev/null); "
            "J=$(journalctl -u ttos-dashboard -b --no-pager 2>/dev/null | wc -l); "
            "A=$(systemctl is-active ttos-dashboard); "
            "N=$(systemctl show ttos-dashboard -p NRestarts --value); "
            f"echo \"{{\\\"pid\\\":$P,\\\"cpu\\\":$S,\\\"rss\\\":$R,\\\"log\\\":$J,\\\"active\\\":\\\"$A\\\",\\\"restarts\\\":$N}}\""))
        try:
            return json.loads(raw)
        except (json.JSONDecodeError, ValueError):
            return None

    import atexit
    atexit.register(iface_up, args.iface)

    first = sample()
    if not first:
        log("  D11: could not sample the car; aborting this soak")
        iface_up(args.iface)
        return {"name": "D11", "ok": False, "reason": "no sample"}
    log(f"  baseline: pid={first['pid']} rss={first['rss']}kB log={first['log']} lines "
        f"restarts={first['restarts']}")

    deadline = time.monotonic() + args.d11_hours * 3600
    worst = dict(first)
    last = first
    problems = []
    while time.monotonic() < deadline:
        time.sleep(min(300, max(30, (deadline - time.monotonic()) / 4)))
        s = sample()
        if not s:
            problems.append("car became unreachable")
            break
        # Take the PID from systemd's MainPID, not pgrep -- pgrep can match a
        # transient sudo/sh wrapper and silently sample the wrong process.
        #
        # A CHANGED PID IS A FAILURE, and it is the failure this soak is for: the
        # service died and systemd restarted it. Without this check it surfaces only
        # as a negative CPU delta (counters reset with the new process), which reads
        # as a measurement glitch rather than a crash.
        if s["pid"] != first["pid"]:
            problems.append(f"process replaced: pid {first['pid']} -> {s['pid']} "
                            f"(it crashed and was restarted)")
            break
        d_cpu = s["cpu"] - last["cpu"]
        if s["active"] != "active":
            problems.append(f"service not active: {s['active']}")
        if s["restarts"] > first["restarts"]:
            problems.append(f"service restarted {s['restarts'] - first['restarts']}x")
        if d_cpu > 6000:                                  # >60 s CPU between samples
            problems.append(f"CPU spin: {d_cpu} ticks in one interval")
        worst["rss"] = max(worst["rss"], s["rss"])
        worst["log"] = s["log"]
        last = s
        left = (deadline - time.monotonic()) / 3600
        log(f"  D11: rss={s['rss']}kB log={s['log']} cpu+{d_cpu} "
            f"restarts={s['restarts']} ({left:.1f} h left)")

    grew_rss = worst["rss"] - first["rss"]
    grew_log = worst["log"] - first["log"]
    if grew_log > 2000:
        problems.append(f"log grew {grew_log} lines")
    if grew_rss > 20000:
        problems.append(f"RSS grew {grew_rss} kB")

    iface_up(args.iface)
    log(f"  {args.iface} restored")

    ok = not problems
    log(f"D11 {'PASS' if ok else 'FAIL'}: RSS +{grew_rss} kB, log +{grew_log} lines")
    for p in problems:
        log(f"    {p}")
    return {"name": "D11", "ok": ok, "rss_growth_kb": grew_rss, "log_growth": grew_log,
            "problems": problems}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--car", default="02")
    ap.add_argument("--host", default="192.168.4.190")
    ap.add_argument("--iface", default="ttcardiag")
    ap.add_argument("--c5-minutes", type=float, default=30)
    ap.add_argument("--d11-hours", type=float, default=4)
    args = ap.parse_args()

    car = load_car(args.car)
    pw = operator_password(args.car)
    log(f"soaking car {args.car} ({car['hostname']}) at {args.host} via {args.iface}")

    results = [soak_c5(args, car, args.host, pw)]
    results.append(soak_d11(args, car, args.host, pw))

    print()
    bad = [r for r in results if not r["ok"]]
    for r in results:
        mark = f"{GRN}PASS{RST}" if r["ok"] else f"{RED}FAIL{RST}"
        print(f"  [{mark}] {r['name']}  {r}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
