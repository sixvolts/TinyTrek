#!/usr/bin/env python3
"""Prove the bench harness itself is trustworthy, before trusting any result.

The bench doc mandates one test: show that the observer does not see harness-
injected frames on the same interface, and treat every negative assertion as
unproven if it is missing.

That test alone is not enough, and the gap is worth naming: a broken observer
that receives NOTHING passes it perfectly. "I did not see the injected frame"
and "I cannot see anything" are the same observation. So this adds POSITIVE
CONTROLS on both sides:

  T1  the injected frame IS visible to a default-loopback socket
        -> the injection really happened; T2 is not vacuous
  T2  the injected frame is NOT visible to an Observer on the same interface
        -> the loopback trap is actually closed
        NB: this failed on first run against an Observer built exactly as the
        doc prescribes. See ttoscan.py's module docstring -- clearing
        CAN_RAW_LOOPBACK on a receiving socket does nothing, because the option
        governs the SENDING socket's echo.
  T3  real wire traffic (the DUT's 0x100 heartbeat) IS visible to an Observer
        -> clearing loopback did not simply deafen the socket
  T4  0x100 does not appear on DIAG
        -> the adapters are not swapped and the buses are not shorted

T1 and T3 are what make T2 mean something. Run this after any rewire, any replug,
and at the top of every test session.

  ./harness-selftest.py
"""

import os
import socket
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "lib"))

import ttoscan  # noqa: E402
from ttoscan import DIAG, DRIVE, Frame, Injector, Observer  # noqa: E402

PROBE_ID = 0x7FF          # unused by the vehicle; safe to inject
HEARTBEAT_ID = 0x100      # DUT dashboard liveness, 200 ms, DRIVE bus only

RED, GRN, YLW, RST = "\033[31m", "\033[32m", "\033[33m", "\033[0m"

results = []


def check(name, ok, detail=""):
    results.append((name, ok))
    mark = f"{GRN}PASS{RST}" if ok else f"{RED}FAIL{RST}"
    print(f"  [{mark}] {name}")
    if detail:
        for line in detail.strip().splitlines():
            print(f"         {line}")
    return ok


def main():
    probe_payload = os.urandom(4)
    is_probe = lambda f: f.can_id == PROBE_ID and f.data == probe_payload
    is_hb = lambda f: f.can_id == HEARTBEAT_ID

    print(f"\nbench harness self-test   DRIVE={DRIVE}  DIAG={DIAG}\n")

    for iface in (DRIVE, DIAG):
        try:
            socket.if_nametoindex(iface)
        except OSError:
            print(f"{RED}interface {iface} does not exist -- run bench-up.sh first{RST}")
            return 2

    # -- T1 / T2 ------------------------------------------------------------
    # Both observers watch the same interface across the same injection, so the
    # two results are directly comparable rather than two separate experiments.
    plain = ttoscan._bind(DRIVE)          # default loopback: SHOULD see it
    plain.setblocking(False)
    obs = Observer(DRIVE)                 # loopback cleared: should NOT see it

    obs.drain()
    while True:                            # drain the plain socket too
        try:
            plain.recv(ttoscan.CANFD_FRAME_SZ)
        except OSError:
            break

    with Injector(DRIVE) as inj:
        try:
            inj.send(Frame(PROBE_ID, probe_payload))
        except OSError as e:
            print(f"{RED}could not inject on {DRIVE}: {e}{RST}")
            print(f"{YLW}ENOBUFS here means no other node is ACKing on the drive bus.{RST}")
            print(f"{YLW}Check the DUT is powered, its candrive is up, and the bus is terminated.{RST}")
            return 2

    time.sleep(0.3)

    seen_plain = False
    while True:
        try:
            f = Frame.unpack(plain.recv(ttoscan.CANFD_FRAME_SZ), iface=DRIVE)
        except OSError:
            break
        if is_probe(f):
            seen_plain = True
    plain.close()

    check("T1  injected frame IS seen by a default-loopback socket", seen_plain,
          "" if seen_plain else
          "The probe never reached any local socket, so T2 proves nothing.\n"
          "Suspect the interface is down or the injector failed silently.")

    leaked = obs.collect(0.4, match=is_probe)
    check("T2  injected frame is NOT seen by the Observer (loopback closed)",
          not leaked,
          "" if not leaked else
          f"Observer saw its own harness traffic: {leaked[0]!r}\n"
          "CAN_RAW_LOOPBACK is not being cleared. EVERY same-bus negative\n"
          "assertion is invalid until this passes.")

    # -- T3 -----------------------------------------------------------------
    obs.drain()
    hb = obs.wait_for(1.5, is_hb)
    check("T3  real wire traffic IS seen by the Observer (positive control)",
          hb is not None,
          "" if hb else
          "No 0x100 heartbeat on the drive bus within 1.5 s.\n"
          "Either the DUT is not running ttos-dashboard, or the Observer is deaf --\n"
          "and if it is deaf, T2 passed for the wrong reason and means nothing.")
    obs.close()

    # -- T4 -----------------------------------------------------------------
    with Observer(DIAG) as dobs:
        dobs.drain()
        stray = dobs.collect(1.5, match=is_hb)
    check("T4  0x100 does NOT appear on DIAG (buses are distinct)", not stray,
          "" if not stray else
          "Drive-bus traffic is reaching the diagnostic bus.\n"
          "Either the adapters are swapped, the two buses are physically shorted,\n"
          "or a gateway rule is forwarding drive traffic outbound (matrix G2).")

    failed = [n for n, ok in results if not ok]
    print()
    if failed:
        print(f"{RED}{len(failed)} of {len(results)} checks failed. "
              f"Bench results are NOT trustworthy until these pass.{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} checks passed -- harness isolation verified{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
