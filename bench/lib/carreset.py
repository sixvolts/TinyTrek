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
         f"echo '{DUT_PASS}' | sudo -S -p '' ttos-reset 2>&1"],
        capture_output=True, text=True, timeout=90)
    if "LOCKED" not in r.stdout and "cleared" not in r.stdout:
        sys.exit(
            "\033[31mcould not reset the car -- refusing to run detection tests"
            "\033[0m\n"
            f"  ssh said: {(r.stdout or r.stderr).strip().splitlines()[-1:] or ['(nothing)']}\n"
            "  If this car is PROVISIONED the factory password no longer works:\n"
            "    export DUT_PASS=<console password, provisioning/OPERATOR-SECRETS.md>\n"
            "  Without a reset the detectors stay disarmed and every flag assertion\n"
            "  below would fail for a reason that is correct behaviour.")
    time.sleep(wait)
    return True
