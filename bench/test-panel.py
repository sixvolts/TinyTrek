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


def session_cookie(jar):
    """(value, httponly) for ttos_session, read from the JAR the client stores.

    Deliberately not the Set-Cookie header: once a client holds a valid session
    the server stops re-issuing the header, so header-scraping finds nothing and
    any assertion built on it passes for the wrong reason. The jar is also
    literally what the browser keeps, which is what P3 is about.
    """
    txt = subprocess.run(SSH + [f"cat {jar}"], capture_output=True, text=True).stdout
    line = next((l for l in txt.splitlines() if "ttos_session" in l), "")
    # curl's jar marks HttpOnly entries with this filename-style prefix.
    return (line.split()[-1] if line else ""), line.startswith("#HttpOnly_")


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
    ap.add_argument("--reboot", action="store_true",
                    help="also run P9: reboot the car and prove it comes back LOCKED "
                         "(~90 s, and it reboots the DUT)")
    args = ap.parse_args()
    car = load_car(args.car)

    print(f"\npanel unlock tiers (P1-P8)   car {car['car_id']}\n")
    A, B = "/tmp/jarA", "/tmp/jarB"
    subprocess.run(SSH + [f"rm -f {A} {B}"], capture_output=True)

    # ---- P1: a locked panel exposes no raw frame tabs -----------------------
    info = jget("/api/info", jar=A)
    check("P1  locked panel exposes no raw frame tabs",
          info.get("frames") == [] and info.get("tier", 0) == 0,
          info=f"frames={info.get('frames')} tier={info.get('tier')}")

    # ---- P3: the cookie is an opaque handle, not a serialisation of state ----
    #
    # THE OLD CHECK SCANNED THE TOKEN FOR SUBSTRINGS -- "tier", "c1", "c2", "c3",
    # "unlock", "admin" -- and failed if it found one. That tests luck, not the
    # product. The token is 32 random bytes in base64url, so a few percent of
    # sessions contain "c1", "c2" or "c3" somewhere by chance. It failed exactly
    # that way on 2026-08-04, on the value ZlmZVlYbekHWzSr7VZwMRr7S8PRkW3ZBc3f_...
    # -- the "Bc3f" near the end. Nothing was leaking.
    #
    # The property worth asserting is that the tier is not in the cookie AT ALL,
    # and the way to establish that is to change the tier and watch the token not
    # move. A byte-identical token across a redemption proves the client holds a
    # handle and the state is server-side. No substring scan can show that, and no
    # random token can fake it.
    #
    # Run on a THROWAWAY session so A and B are undisturbed. C1 does not touch the
    # car-level record (only C2 and C3 do, because only they have detectors to stand
    # down), so redeeming it here leaves nothing for P7 to trip over.
    C = "/tmp/jarC"
    subprocess.run(SSH + [f"rm -f {C}"], capture_output=True)
    jget("/api/info", jar=C)
    tok_before, httponly = session_cookie(C)
    redeem(car["code_c1"], C)
    tier_c = jget("/api/info", jar=C).get("tier")
    tok_after, _ = session_cookie(C)
    structured = any(ch in tok_before for ch in ".=:|{")   # JWT / k=v / JSON shapes
    check("P3  cookie is an opaque handle: unchanged by a tier change, HttpOnly",
          bool(tok_before) and httponly and not structured
          and tok_before == tok_after and tier_c == 1,
          info=f"token {tok_before[:12]}… ({len(tok_before)} chars) HttpOnly={httponly}; "
               f"tier 0 -> {tier_c}, token "
               f"{'unchanged' if tok_before == tok_after else 'CHANGED — state may be client-side'}")

    # ---- P2: a valid code unlocks its tier; a wrong one changes nothing ------
    #
    # THIS USED TO REDEEM ANOTHER CAR'S C2 CODE and require rejection. That check
    # is gone because the property is gone: the fleet is deliberately uniform --
    # every car carries the same C1/C2/C3 codes and the same Data IDs, so that a
    # team forced onto a different car mid-event keeps its progress and so that
    # one firmware image serves all eight. Another car's code IS this car's code;
    # asserting it is refused would fail a correct system.
    #
    # The negative that still means something is a well-formed code that is simply
    # not one of the three. That is what a contestant actually submits when they
    # get the CRC-8 wrong, and it must not move the tier.
    r1 = redeem(car["code_c1"], A)
    t_after_c1 = jget("/api/info", jar=A).get("tier")
    r_bad = redeem("9ZZZZZZZ", A)
    t_after_bad = jget("/api/info", jar=A).get("tier")
    check("P2  own C1 code unlocks tier 1; a wrong code is rejected and does not "
          "disturb the tier already held",
          r1.get("ok") is True and t_after_c1 == 1
          and r_bad.get("ok") is False and t_after_bad == 1,
          info=f"C1 -> ok={r1.get('ok')} tier={t_after_c1}; "
               f"9ZZZZZZZ -> ok={r_bad.get('ok')} tier={t_after_bad}")

    # DIAG tab appears at tier 1, DRIVE does not. The panel reports REAL interface
    # names, so this is the role name from 10-ttos-can.rules -- it read ["can0"]
    # until 2026-08-04, which was both a dead name and the pre-correction role.
    frames1 = jget("/api/info", jar=A).get("frames", [])
    check("P2b tier 1 reveals the DIAG bus tab only",
          frames1 == ["candiag"], info=f"frames={frames1}")

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

    # ---- P6c/P6d: the WIRE, not the reply ------------------------------------
    #
    # THIS IS THE GAP THAT HID A DEAD E-STOP. P6b asks only whether /api/control
    # ACCEPTED the request. On a provisioned car the handler used to answer
    # {"ok":true} while the frames went nowhere: the control pad wrote through a
    # second socket gated by TTOS_DASH_DRIVE, which ttos-provision emptied on every
    # competition car. Motors kept their buffered steps, the 12V rail stayed up,
    # and the panel showed success. The suite was green over it.
    #
    # An HTTP 200 is not propulsion. Watch the DRIVE bus.
    sys.path.insert(0, os.path.join(HERE, "lib"))
    from ttoscan import DRIVE, Observer  # noqa: E402

    # A tier-3 session must actually put motor commands on the bus.
    with Observer(DRIVE) as od:
        od.drain()
        curl("/api/control", jar=A, method="POST", data='{"cmd":"forward"}')
        moved = od.collect(1.2, match=lambda f: f.can_id in (0x111, 0x113))
    ids_moved = sorted({hex(f.can_id) for f in moved})
    check("P6c tier-3 'forward' puts BOTH motor commands on the DRIVE bus",
          {0x111, 0x113} <= {f.can_id for f in moved},
          info=f"saw {len(moved)} frames {ids_moved} (want 0x111 and 0x113)")

    # And the e-stop must reach the wire from a LOCKED session -- both step buffers
    # zeroed and the 12V rail dropped. Checking only that it was accepted is what
    # let this rot.
    with Observer(DRIVE) as od:
        od.drain()
        curl("/api/control", jar=B, method="POST", data='{"cmd":"stop"}')
        est = od.collect(1.2, match=lambda f: f.can_id in (0x111, 0x113, 0x115))
    zeroed = {f.can_id for f in est
              if f.can_id in (0x111, 0x113) and len(f.data) > 4 and f.data[4] == 0x00}
    power_off = any(f.can_id == 0x115 and f.data[:1] == b"\x02" for f in est)
    check("P6d e-stop from a LOCKED session reaches the bus (buffers zeroed, 12V cut)",
          zeroed == {0x111, 0x113} and power_off,
          info=f"zeroed={sorted(hex(i) for i in zeroed)} (want 0x111+0x113), "
               f"12V-off frame seen={power_off}")

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

    # The car-level unlock record used to be readable over /api/judge. That endpoint
    # is gone -- it was never asked for, and it handed anyone on the AP a
    # solved/not-solved readout. The record itself still exists and is still
    # load-bearing: it drives the heartbeat ARM MASK, which is what P8 above and P7
    # below assert against. The mask IS the observable, and it is a better one,
    # because it is what the BMS actually acts on.

    # ---- P7: reset clears everything ----------------------------------------
    pw = os.environ.get("DUT_PASS", "ttos")
    subprocess.run(SSH + [f"echo '{pw}' | sudo -S -p '' ttos-reset >/dev/null 2>&1; true"],
                   capture_output=True, timeout=60)
    time.sleep(3)
    infoA = jget("/api/info", jar=A)
    with Observer(DRIVE) as od:
        od.drain()
        hb2 = od.collect(1.5, match=lambda f: f.can_id == 0x100)
    masks2 = {f.data[1] for f in hb2 if len(f.data) >= 2}
    check("P7  reset clears every unlock and re-arms the detectors",
          infoA.get("tier", 0) == 0 and masks2 == {0x03},
          info=f"session A tier={infoA.get('tier')}, "
               f"arm masks={[hex(m) for m in sorted(masks2)]} (want 0x03)")

    # ---- P9: a reboot returns the car to LOCKED ------------------------------
    #
    # Opt-in because it reboots the car and costs ~90 s. The property is worth an
    # explicit test rather than an argument from "it is all in memory": that
    # reasoning would stop being true the moment anyone persists unlock state to
    # make it survive a crash, and the failure would be silent -- a car handed to
    # the next team already unlocked, with its detectors stood down, looking normal.
    #
    # Three things have to be true afterwards, and the third is the one an
    # in-memory argument does not cover on its own:
    #   1. a fresh session is tier 0 with no frame tabs
    #   2. the heartbeat arm mask is back to 0x03, so the BMS is detecting again
    #   3. a cookie held from BEFORE the reboot is worthless -- the client keeps it,
    #      but the server has forgotten it, so it cannot carry an unlock across
    if args.reboot:
        pw = os.environ.get("DUT_PASS", "ttos")
        # Unlock everything first, or the check passes on a car that was already locked.
        for code in (car["code_c1"], car["code_c2"], car["code_c3"]):
            redeem(code, A)
        before = jget("/api/info", jar=A).get("tier")
        subprocess.run(SSH + [f"echo '{pw}' | sudo -S -p '' systemctl reboot"],
                       capture_output=True, timeout=30)
        time.sleep(20)
        for _ in range(40):
            up = subprocess.run(SSH + ["cut -d. -f1 /proc/uptime"],
                                capture_output=True, text=True, timeout=15).stdout.strip()
            if up.isdigit() and int(up) < 300:
                break
            time.sleep(5)
        time.sleep(10)   # let the dashboard bind and the heartbeat start

        fresh = "/tmp/jarR"
        subprocess.run(SSH + [f"rm -f {fresh}"], capture_output=True)
        info_new = jget("/api/info", jar=fresh)
        info_old = jget("/api/info", jar=A)        # the pre-reboot cookie
        with Observer(DRIVE) as od:
            od.drain()
            hb3 = od.collect(2.0, match=lambda f: f.can_id == 0x100)
        masks3 = {f.data[1] for f in hb3 if len(f.data) >= 2}
        check("P9  a reboot returns the car to LOCKED, and old cookies do not survive",
              before == 3 and info_new.get("tier", 9) == 0 and info_new.get("frames") == []
              and info_old.get("tier", 9) == 0 and masks3 == {0x03},
              info=f"tier before reboot={before}; after: fresh session tier="
                   f"{info_new.get('tier')} frames={info_new.get('frames')}, "
                   f"pre-reboot cookie tier={info_old.get('tier')}, "
                   f"arm masks={[hex(m) for m in sorted(masks3)]} (want 0x03)")

    bad = [n for n, ok in results if not ok]
    print()
    if bad:
        print(f"{RED}{len(bad)} of {len(results)} failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} passed{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
