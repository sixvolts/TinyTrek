"""Return the car to a locked state before asserting anything about detection.

Every detection assertion depends on the BMS detectors being ARMED, and they are
armed only while the car's unlock record is empty -- redeeming C2 or C3 stands them
down on purpose (matrix P8). So a suite run after any test that redeemed a code
sees zero flags and reports a detection failure that is actually correct behaviour.

That is the same trap an operator hits between rounds: a car left unlocked by the
previous team emits no flags for the next one. ttos-reset exists for it; the suites
should use it rather than assume.
"""

import os
import subprocess

DUT = os.environ.get("DUT", "192.168.4.133")
DUT_USER = os.environ.get("DUT_USER", "ttos")
DUT_PASS = os.environ.get("DUT_PASS", "ttos")


def reset_car(wait=4.0):
    """Clear all unlocks and re-arm the detectors.

    FAILS LOUDLY. A reset that silently does not happen -- the usual cause being
    DUT_PASS still set to the factory password on a provisioned car -- leaves the
    detectors stood down, and every detection assertion then fails for a reason
    that is correct behaviour. That wastes far more time than an abort here.
    """
    import sys
    import time
    r = subprocess.run(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8",
         f"{DUT_USER}@{DUT}",
         f"echo '{DUT_PASS}' | sudo -S -p '' ttos-reset 2>&1; echo EXIT=$?"],
        capture_output=True, text=True, timeout=90)

    # KEY ON THE EXIT CODE, NOT THE PROSE. This used to accept "LOCKED" OR
    # "cleared" -- and ttos-reset printed "unlocks : cleared" unconditionally,
    # before it verified anything. A reset that left the gateway empty (so no flag
    # frame can cross DRIVE->DIAG, so no challenge can ever award a code) matched
    # on "cleared" and the suite ran on happily. ttos-reset now verifies the
    # dashboard, the gateway and the rule count, and exits non-zero if any of them
    # is wrong; that verdict is what this must read.
    ok = "EXIT=0" in r.stdout and "LOCKED" in r.stdout
    if not ok:
        tail = "\n    ".join((r.stdout or r.stderr).strip().splitlines()[-6:]) or "(nothing)"
        sys.exit(
            "\033[31mcould not reset the car -- refusing to run detection tests"
            "\033[0m\n"
            f"  ttos-reset said:\n    {tail}\n"
            "  If this car is PROVISIONED the factory password no longer works:\n"
            "    export DUT_PASS=<console password, provisioning/OPERATOR-SECRETS.md>\n"
            "  If the GATEWAY is the complaint, flag frames cannot reach the\n"
            "  diagnostic bus and no challenge can award a code on this car.\n"
            "  Without a reset the detectors stay disarmed and every flag assertion\n"
            "  below would fail for a reason that is correct behaviour.")
    time.sleep(wait)
    return True
