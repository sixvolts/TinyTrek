#!/usr/bin/env python3
"""Exercise the C2/C3 detection rules against a running vehicle-emulator.

Frames are injected directly on the DRIVE bus, so this tests the RULES without
needing the DUT to be unlocked or the panel to be driven. Run the emulator first:

    ./vehicle-emulator.py --car 01 &
    ./test-detectors.py

The negative cases carry the weight. C5 ("30 minutes of legitimate operation emits
zero flags") is the assertion that fails quietly, and every case below that ends in
"-> expect SILENCE" is a miniature of it.
"""

import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "lib"))

from ttoscan import DRIVE, Frame, Injector  # noqa: E402

ID_L, ID_R = 0x111, 0x113
# Must track TTOS_PIVOT_RPM / the BMS PIVOT_RPM. Frames carrying this rpm are the
# ones a C2 solver replays, and they must never reach C3.
PIVOT_RPM = 75
DRIVE_RPM = 100      # what a contestant would plausibly teleoperate at
FWD, REV = 0x01, 0x02
HERE = os.path.dirname(os.path.abspath(__file__))

RED, GRN, RST = "\033[31m", "\033[32m", "\033[0m"
results = []


def ctl(*a):
    return subprocess.run([os.path.join(HERE, "emu-ctl.sh"), *a],
                          capture_output=True, text=True).stdout.strip()


def counts():
    """(c2_fires, c3_fires) from the emulator's own counters.

    Read from the emulator rather than from a DRIVE Observer on purpose: the
    Observer drops locally generated frames (MSG_DONTROUTE), and the emulator's
    transmissions ARE local to this host, so a bench-side DRIVE observer cannot
    see them. That is the loopback protection working correctly, not a fault --
    but it means "did the emulated BMS emit a flag?" is answered by its counters,
    while "did the flag cross the gateway?" is answered by a DIAG Observer.
    """
    for line in ctl("status").splitlines():
        if line.startswith("flags emitted:"):
            p = line.split()
            return int(p[3].lstrip("x")), int(p[5].lstrip("x"))
    raise SystemExit("could not read emulator status -- is it running?")


def cmd(inj, can_id, steps, d, rpm):
    inj.send(Frame(can_id, bytes([(steps >> 24) & 0xFF, (steps >> 16) & 0xFF,
                                  (steps >> 8) & 0xFF, steps & 0xFF, d, rpm])))


def case(name, fn, want_c2, want_c3):
    ctl("reset")
    time.sleep(0.6)
    before = counts()
    with Injector(DRIVE) as inj:
        fn(inj)
    time.sleep(1.2)                      # let 2 Hz emission land
    after = counts()
    got_c2, got_c3 = after[0] - before[0], after[1] - before[1]
    ok = ((got_c2 > 0) == want_c2) and ((got_c3 > 0) == want_c3)
    results.append(ok)
    mark = f"{GRN}PASS{RST}" if ok else f"{RED}FAIL{RST}"
    print(f"  [{mark}] {name}")
    print(f"         0x7D1 x{got_c2} (want {'>0' if want_c2 else '0'})   "
          f"0x7D2 x{got_c3} (want {'>0' if want_c3 else '0'})")


# ---------------------------------------------------------------- scenarios ---

def pivot_legit(inj):
    """The sanctioned C1 routine: opposite dir, the pivot rpm, one command per wheel."""
    for _ in range(10):
        cmd(inj, ID_L, 102, FWD, PIVOT_RPM)
        cmd(inj, ID_R, 102, REV, PIVOT_RPM)
        time.sleep(0.15)


def pivot_at_100(inj):
    """The same pivot if its rpm is ever raised off 50 to escape the resonance
    band. Opposite dir, so it must STILL be silent -- this is the case that an
    rpm-only C3 rule gets wrong."""
    for _ in range(20):
        cmd(inj, ID_L, 102, FWD, 100)
        cmd(inj, ID_R, 102, REV, 100)
        time.sleep(0.08)


def single_wheel(inj):
    """One wheel only. Never a translation, so never C2."""
    for _ in range(12):
        cmd(inj, ID_L, 100, FWD, 100)
        time.sleep(0.1)


def brief_translation(inj):
    """Both wheels, same dir, but only a couple of commands: C2 yes, C3 no."""
    for _ in range(2):
        cmd(inj, ID_L, 100, FWD, PIVOT_RPM)
        cmd(inj, ID_R, 100, FWD, PIVOT_RPM)
        time.sleep(0.15)


def composed_replay(inj):
    """The C2 solution technique: captured pivot frames replayed same-dir. They
    structurally carry the PIVOT rpm, so this must reach C2 and NEVER C3 (C7)."""
    for _ in range(25):
        cmd(inj, ID_L, 102, FWD, PIVOT_RPM)
        cmd(inj, ID_R, 102, FWD, PIVOT_RPM)
        time.sleep(0.08)


def composed_replay_of_100rpm_pivot(inj):
    """THE HAZARD, made executable.

    If the C1 pivot's rpm is ever raised off 50 to escape the stepper resonance
    band, the frames a contestant captures carry that rpm -- so replaying them
    same-dir (the C2 solution) becomes indistinguishable from forged teleoperation
    and trips C3 as well. Matrix C7 says that must not happen.

    This case asserts that it DOES trip, because that is the truth under a 100 rpm
    pivot. It is a regression guard on the constraint, not on the code: if someone
    changes TTOS_PIVOT_RPM to 100 and this case starts looking benign, the tier
    separation has been silently redefined.
    """
    for _ in range(25):
        cmd(inj, ID_L, 102, FWD, 100)
        cmd(inj, ID_R, 102, FWD, 100)
        time.sleep(0.08)


def sustained_forged(inj):
    """C3: sustained same-dir commanding at rpm=100."""
    for _ in range(25):
        cmd(inj, ID_L, 100, FWD, 100)
        cmd(inj, ID_R, 100, FWD, 100)
        time.sleep(0.08)


def stop_breaks_run(inj):
    """A stop between qualifying commands breaks 'consecutive', so the C3 run
    should never accumulate to 15."""
    for _ in range(25):
        cmd(inj, ID_L, 100, FWD, 100)
        cmd(inj, ID_R, 100, FWD, 100)
        cmd(inj, ID_L, 0, 0x00, 100)
        time.sleep(0.08)


def main():
    print("\nC2 / C3 detection rules\n")
    #     name                                        scenario           C2    C3
    case(f"legit pivot (opposite dir, rpm={PIVOT_RPM}) -> expect SILENCE",
         pivot_legit, False, False)
    case("pivot raised to rpm=100 (opposite dir) -> expect SILENCE",
         pivot_at_100, False, False)
    case("single wheel only -> expect SILENCE",
         single_wheel, False, False)
    case("brief same-dir translation -> C2 only",
         brief_translation, True, False)
    case(f"composed replay at the pivot rpm ({PIVOT_RPM}) -> C2 only (matrix C7)",
         composed_replay, True, False)
    case(f"HAZARD: if the pivot ran at {DRIVE_RPM}, composed replay would trip C3",
         composed_replay_of_100rpm_pivot, True, True)
    case("sustained forged same-dir at rpm=100 -> C2 and C3",
         sustained_forged, True, True)
    case("stop between commands breaks the run -> C2 only",
         stop_breaks_run, True, False)

    ctl("reset")
    bad = results.count(False)
    print()
    if bad:
        print(f"{RED}{bad} of {len(results)} cases failed{RST}\n")
        return 1
    print(f"{GRN}all {len(results)} cases passed{RST}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
