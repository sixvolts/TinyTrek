#!/usr/bin/env python3
"""Panel unlock tiers -- bench brief section 6, P1 through P8.

Runs against the car's HTTP console. The panel binds the AP address, so requests
are made from the DUT itself, exactly like test-relay.sh.

    ./test-panel.py --car 01

P3-P6 are the security-shaped ones and the reason this is worth doing carefully:
it is easy to gate the UI and leave the endpoint open, which is P5 precisely.
"""

import argparse
import csv
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"
results = []

DUT = os.environ.get("DUT", "192.168.4.133")
DUT_USER = os.environ.get("DUT_USER", "ttos")
SSH = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", f"{DUT_USER}@{DUT}"]
BASE = "http://192.168.244.1"


def check(name, ok, info=""):
    results.append((name, ok))
    print(f"  [{GRN}PASS{RST}] {name}" if ok else f"  [{RED}FAIL{RST}] {name}")
    for line in str(info).strip().splitlines():
        if line:
            print(f"         {line}")


def curl(path, jar=None, method="GET", data=None, extra=""):
    """Run curl ON THE CAR. jar is a cookie-jar filename to persist a session."""
    cmd = ["curl", "-s", "--max-time", "8"]
    if jar:
        cmd += ["-c", jar, "-b", jar]
    if method == "POST":
        cmd += ["-X", "POST", "-H", "Content-Type: application/json", "-d", f"'{data}'"]
    if extra:
        cmd += [extra]
    cmd += [f"{BASE}{path}"]
    r = subprocess.run(SSH + [" ".join(cmd)], capture_output=True, text=True, timeout=40)
    return r.stdout


def curl_headers(path, jar=None):
    r = subprocess.run(SSH + [f"curl -s -D - -o /dev/null --max-time 8 "
                              f"{'-c ' + jar + ' -b ' + jar + ' ' if jar else ''}{BASE}{path}"],
                       capture_output=True, text=True, timeout=40)
    return r.stdout


def jget(path, jar=None):
    try:
        return json.loads(curl(path, jar=jar) or "{}")
    except json.JSONDecodeError:
        return {}


def redeem(code, jar):
    return jget_post("/api/flag", f'{{"code":"{code}"}}', jar)


def jget_post(path, data, jar):
    try:
        return json.loads(curl(path, jar=jar, method="POST", data=data) or "{}")
    except json.JSONDecodeError:
        return {}


def load_car(car_id):
    with open(os.path.join(HERE, "..", "provisioning", "fleet-table.csv"), newline="") as fh:
        for row in csv.DictReader(fh):
            row = {k: (v or "").strip() for k, v in row.items()}
            if row["car_id"] == car_id:
                return row
    raise SystemExit(f"car {car_id} not in fleet-table.csv")


def reset():
    subprocess.run(SSH + ["sudo -n ttos-reset >/dev/null 2>&1 || "
                          "systemctl --user true 2>/dev/null; true"],
                   capture_output=True, timeout=60)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--car", default="01")
    args = ap.parse_args()
    car = load_car(args.car)
    other = load_car("02" if args.car != "02" else "03")

    print(f"\npanel unlock tiers (P1-P8)   car {car['car_id']}\n")
    A, B = "/tmp/jarA", "/tmp/jarB"
    subprocess.run(SSH + [f"rm -f {A} {B}"], capture_output=True)

    # ---- P1: a locked panel exposes no raw frame tabs -----------------------
    info = jget("/api/info", jar=A)
    check("P1  locked panel exposes no raw frame tabs",
          info.get("frames") == [] and info.get("tier", 0) == 0,
          info=f"frames={info.get('frames')} tier={info.get('tier')}")

    # ---- P3: no unlock state in any client-visible cookie value -------------
    # Inspect the COOKIE JAR, not a Set-Cookie header: once the client holds a
    # valid session the server stops re-issuing the header, so header-scraping
    # silently finds nothing and the check passes for the wrong reason. The jar is
    # also literally what the client stores, which is what the assertion is about.
    jar_txt = subprocess.run(SSH + [f"cat {A}"], capture_output=True, text=True).stdout
    line = next((l for l in jar_txt.splitlines() if "ttos_session" in l), "")
    cookie_val = line.split()[-1] if line else ""
    httponly = line.startswith("#HttpOnly_")   # curl's jar marks it with this prefix
    leaky = any(t in cookie_val.lower() for t in ("tier", "c1", "c2", "c3", "unlock", "admin"))
    check("P3  no unlock state in the cookie value (opaque token, HttpOnly)",
          bool(cookie_val) and not leaky and httponly,
          info=f"cookie={cookie_val[:16]}… ({len(cookie_val)} chars), "
               f"HttpOnly={httponly}")

    # ---- P2: each code unlocks exactly its tier; another car's is rejected ---
    r1 = redeem(car["code_c1"], A)
    t_after_c1 = jget("/api/info", jar=A).get("tier")
    r_other = redeem(other["code_c2"], A)
    t_after_other = jget("/api/info", jar=A).get("tier")
    check("P2  own C1 code unlocks tier 1; another car's code is rejected",
          r1.get("ok") is True and t_after_c1 == 1
          and r_other.get("ok") is False and t_after_other == 1,
          info=f"C1 -> ok={r1.get('ok')} tier={t_after_c1}; "
               f"car {other['car_id']}'s C2 -> ok={r_other.get('ok')} tier={t_after_other}")

    # DIAG tab appears at tier 1, DRIVE does not.
    frames1 = jget("/api/info", jar=A).get("frames", [])
    check("P2b tier 1 reveals the DIAG bus tab only",
          frames1 == ["can0"], info=f"frames={frames1}")

    # ---- P4: a second browser session does not inherit ----------------------
    infoB = jget("/api/info", jar=B)
    check("P4  a second session does not inherit the first's unlock",
          infoB.get("tier", 0) == 0 and infoB.get("frames") == [],
          info=f"session B tier={infoB.get('tier')} frames={infoB.get('frames')}")

    # ---- P5: the SSE endpoint cannot be driven without the tier -------------
    # Unlock DRIVE frames in session A, then check session B's stream carries none.
    redeem(car["code_c2"], A)
    framesA = jget("/api/info", jar=A).get("frames", [])
    streamB = curl("/events", jar=B, extra="--max-time 4")
    streamA = curl("/events", jar=A, extra="--max-time 4")
    b_has_frames = '"type":"frame"' in streamB
    a_has_frames = '"type":"frame"' in streamA
    check("P5  SSE carries no frames to an unentitled session",
          not b_has_frames,
          info=f"session A (tier 2) frames={framesA}, stream has frames={a_has_frames}; "
               f"session B (tier 0) stream has frames={b_has_frames}")
    check("P5b SSE does carry frames to an entitled session (positive control)",
          a_has_frames,
          info="without this, P5 passes for a stream that is simply empty")

    # ---- P8 setup / P2: C3 ---------------------------------------------------
    r3 = redeem(car["code_c3"], A)
    t3 = jget("/api/info", jar=A).get("tier")
    check("P2c C3 code unlocks tier 3", r3.get("ok") is True and t3 == 3,
          info=f"ok={r3.get('ok')} tier={t3}")

    # ---- P6: drive control is refused once a session lapses ------------------
    # The 10-minute idle is too long to wait for, so exercise the same gate from a
    # session that never had the tier: /api/control must refuse it, while "stop"
    # must always work.
    drive = curl("/api/control", jar=B, method="POST", data='{"cmd":"forward"}')
    stop = curl("/api/control", jar=B, method="POST", data='{"cmd":"stop"}')
    check("P6  drive controls refused to a session without tier 3",
          "locked" in drive.lower() or "forbidden" in drive.lower(),
          info=f"forward -> {drive.strip()[:90]}")
    check("P6b e-stop works from ANY session, locked or expired",
          "locked" not in stop.lower(),
          info=f"stop -> {stop.strip()[:90] or '(accepted)'}")

    # ---- P8: post-C3, detectors stand down -----------------------------------
    time.sleep(1.0)
    sys.path.insert(0, os.path.join(HERE, "lib"))
    from ttoscan import DRIVE, Observer  # noqa: E402
    with Observer(DRIVE) as od:
        od.drain()
        hb = od.collect(1.5, match=lambda f: f.can_id == 0x100)
    masks = {f.data[1] for f in hb if len(f.data) >= 2}
    check("P8  after C3, the heartbeat arm mask stands both detectors down",
          masks == {0x00},
          info=f"arm masks seen: {[hex(m) for m in sorted(masks)]} (want 0x00)")

    # ---- judging record ------------------------------------------------------
    judge = jget("/api/judge")
    check("J   judging endpoint reports the CAR record, not a session",
          judge.get("redeemed") == [2, 3] and judge.get("car") == car["car_id"],
          info=f"{judge}")

    # ---- P7: reset clears everything ----------------------------------------
    pw = os.environ.get("DUT_PASS", "ttos")
    subprocess.run(SSH + [f"echo '{pw}' | sudo -S -p '' ttos-reset >/dev/null 2>&1; true"],
                   capture_output=True, timeout=60)
    time.sleep(3)
    infoA = jget("/api/info", jar=A)
    judge2 = jget("/api/judge")
    with Observer(DRIVE) as od:
        od.drain()
        hb2 = od.collect(1.5, match=lambda f: f.can_id == 0x100)
    masks2 = {f.data[1] for f in hb2 if len(f.data) >= 2}
    check("P7  reset clears every unlock and re-arms the detectors",
          infoA.get("tier", 0) == 0 and judge2.get("redeemed") == [] and masks2 == {0x03},
          info=f"session A tier={infoA.get('tier')}, car record={judge2.get('redeemed')}, "
               f"arm masks={[hex(m) for m in sorted(masks2)]} (want 0x03)")

    bad = [n for n, ok in results if not ok]
    print()
    if bad:
        print(f"{RED}{len(bad)} of {len(results)} failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} passed{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
