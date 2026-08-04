#!/usr/bin/env python3
"""Emulate the TinyTrek drive-bus nodes -- both motors and the BMS -- on the bench.

Bound to the DRIVE adapter. The DUT talks to this instead of to an empty bus, so
the challenge logic can be exercised without a finished vehicle.

    sudo ./bench-up.sh
    ./vehicle-emulator.py --car 01

Runtime control (fault injection) over a unix socket:

    ./emu-ctl.sh status
    ./emu-ctl.sh drop 20          # drop 20% of received frames
    ./emu-ctl.sh lvc on           # latch low-voltage cutoff
    ./emu-ctl.sh vbat 9200        # park the pack voltage
    ./emu-ctl.sh sweep 8300 12400 60
    ./emu-ctl.sh mute on          # stop transmitting entirely

WHAT THIS IS FOR (bench brief section 4): C2/C3 detection and motor-side CRC
validation are specified to live in BMS/motor firmware, which is the slowest thing
to iterate on across eight cars. Prototype the rules here, tune the thresholds
against real timing, then port a validated rule set to the RP2040.

It tests the SPECIFICATION, not the firmware. A correct Python BMS proves the
rules are right; it does not prove the RP2040 implementation matches. One real car
still has to close that loop before the fleet is flashed.

------------------------------------------------------------------------------
FIDELITY NOTE -- where this deliberately DIVERGES from the bench brief

The brief says the emulator must "track 0x100; refuse motion when absent, matching
real node behaviour."

That is not real node behaviour. Read TinytrekLMotor.ino: heartbeatOK() and
powerOK() exist, but they feed updateLed() and NOTHING ELSE. The step-drain block
is gated on `stepsLeft > 0` alone. A motor node with no heartbeat and no beacon
still drains its buffer and pulses STEP.

What actually stops the car is ELECTRICAL: the BMS holds the 12 V rail off, so the
drivers are unpowered. The interlock is in the wiring, not the firmware.

This matters beyond pedantry. An emulator that refuses to move without a heartbeat
would let a challenge design depend on "no heartbeat = no motion", which is false
on the real vehicle -- and the bench would certify it. So this models what the
firmware does (buffer drains unconditionally) and tracks the rail separately:
`moving` is true only when steps remain AND the rail is on. `status` reports both,
so a test can tell "commanded but unpowered" from "not commanded".
------------------------------------------------------------------------------
"""

import argparse
import csv
import os
import random
import select
import socket
import struct
import sys
import threading
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "lib"))

from ttoscan import DRIVE, Frame, Injector, Observer, crc8_j1850  # noqa: E402

# ------------------------------------------------------------------ IDs -------
ID_HEARTBEAT = 0x100   # Pi -> nodes   [seq][armflags]
ID_MOTOR_L = 0x111     # Pi -> left motor
ID_MOTOR_R = 0x113     # Pi -> right motor
ID_POWER = 0x115       # Pi -> BMS     [0x01 on | 0x02 off]
ID_BEACON = 0x116      # BMS -> all    [v_hi][v_lo][pwr][flags]
ID_FLAG_C2 = 0x7D1     # BMS -> bus    8 ASCII
ID_FLAG_C3 = 0x7D2     # BMS -> bus    8 ASCII

DIR_STOP, DIR_FWD, DIR_REV = 0x00, 0x01, 0x02

# ------------------------------------------------- firmware-derived constants -
STEPS_MAX = 250              # TinytrekLMotor.ino
STEPS_PER_REV = 200
BEACON_MS = 200              # TinytrekBMS.ino TELEM_MS -- 5 Hz
HEARTBEAT_TIMEOUT_MS = 1000  # motor + BMS stand-down timeout
LVC_CUTOFF_MV = 8500
LVC_RECOVER_MV = 9000
LVC_TRIP_MS = 1500

# --------------------------------------------------- challenge-spec constants -
ARM_C2, ARM_C3 = 0x01, 0x02
C3_COUNT = 15                # "15 consecutive qualifying commands"
C3_WINDOW_S = 3.0            # "...within 3 s"
# C3 qualifies on any rpm that is NOT the pivot's -- matching TinytrekBMS's
# `rpm != PIVOT_RPM`, NOT an equality test against one hard-coded speed.
#
# The prototype originally keyed on `rpm == 100` while the firmware keyed on
# `!= PIVOT_RPM`, which are not the same rule: a contestant forging at 150 trips
# the firmware and slips past the prototype. The divergence was invisible while the
# pivot was 50 and driving was assumed to be 100, and surfaced only when the pivot
# moved. The BENCH must model the firmware, or it certifies a rule nobody runs.
PIVOT_RPM = 75               # MUST MATCH TTOS_PIVOT_RPM on the Pi and the BMS
FLAG_EMIT_HZ = 2.0           # section D: repeatable emission, not one-shot

# NOT IN THE SPEC -- tuned here, and this is exactly the kind of number the bench
# exists to pin down before it is compiled into eight RP2040s. The layering doc
# says C2 fires when both motors are commanded in the same dir "within a short
# window" and never says how short. The dashboard's keepalive is 150 ms, so both
# wheels are commanded inside one cycle; 250 ms covers that with margin without
# being loose enough to pair two unrelated single-wheel commands.
C2_PAIR_WINDOW_S = 0.25
# How long the C2 condition is considered to still "hold" after the last
# qualifying pair, so emission repeats at 2 Hz through a sustained translation
# rather than firing once per pair.
C2_HOLD_S = 1.0

CTL_SOCK = os.environ.get("TTOS_EMU_SOCK", "/tmp/ttos-bench-emu.sock")


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


# ================================================================== identity ==

def load_car(car_id, csv_path):
    """Per-car challenge identity. The bench must be provisioned as ONE car and
    stay that way -- Data IDs and codes in test fixtures are car-specific, and a
    mismatch presents as universal silent CRC rejection, which looks exactly like
    a bus fault."""
    with open(csv_path, newline="") as fh:
        for row in csv.DictReader(fh):
            # fleet-table.csv is CRLF; DictReader leaves \r on the last field.
            row = {k: (v or "").strip() for k, v in row.items()}
            if row["car_id"] == car_id:
                return row
    raise SystemExit(f"car {car_id!r} not in {csv_path}")


# ==================================================================== motor ===

class Motor:
    """One stepper node. Mirrors TinytrekLMotor.ino's buffer semantics exactly."""

    def __init__(self, name, can_id, data_id):
        self.name = name
        self.can_id = can_id
        self.data_id = data_id
        self.steps_left = 0.0
        self.dir = DIR_FWD
        self.rpm = 100
        self.accepted = 0
        self.rejected = 0
        self.last_cmd = None      # (t, dir, rpm) of the last ACCEPTED command
        self._frac = 0.0

    def command(self, steps, d, rpm):
        """Fold a validated command into the buffer, firmware-for-firmware."""
        self.rpm = rpm if rpm > 0 else self.rpm
        if self.rpm < 1:
            self.rpm = 1
        if d == DIR_STOP:
            self.steps_left = 0.0
        else:
            if d != self.dir:
                self.steps_left = 0.0    # direction change clears
            self.dir = d
            self.steps_left = min(self.steps_left + steps, STEPS_MAX)
        self.accepted += 1
        self.last_cmd = (time.monotonic(), d, self.rpm)

    def drain(self, dt, powered):
        """Consume the buffer at the commanded rpm.

        NOTE: draining is NOT gated on heartbeat or rail state, because the real
        firmware does not gate it either (see the fidelity note at the top).
        `powered` only decides whether the shaft actually turns.
        """
        if self.steps_left <= 0:
            return 0
        steps_per_s = self.rpm / 60.0 * STEPS_PER_REV
        self._frac += steps_per_s * dt
        n = int(self._frac)
        self._frac -= n
        if n:
            self.steps_left = max(0.0, self.steps_left - n)
        return n if powered else 0

    @property
    def moving(self):
        return self.steps_left > 0


# ====================================================================== BMS ===

class BMS:
    def __init__(self, vbat_mv=12100):
        self.vbat_mv = vbat_mv
        self.rail_on = False
        self.lvc = False
        self._below_since = None
        # fault-injection state
        self.sweep = None          # (lo, hi, period_s, t0)

    def on_power_cmd(self, b):
        if b == 0x01:
            if not self.lvc:       # refuse power-on while the pack is too low
                self.rail_on = True
        elif b == 0x02:
            self.rail_on = False

    def tick(self, now):
        if self.sweep:
            lo, hi, period, t0 = self.sweep
            phase = ((now - t0) % period) / period
            tri = phase * 2 if phase < 0.5 else (1 - phase) * 2
            self.vbat_mv = int(lo + (hi - lo) * tri)

        # Latch only after the pack stays below cutoff continuously, matching
        # TinytrekBMS.ino -- motor inrush sag must not cut the rail.
        if self.vbat_mv < LVC_CUTOFF_MV:
            if self._below_since is None:
                self._below_since = now
            elif not self.lvc and (now - self._below_since) * 1000 >= LVC_TRIP_MS:
                self.lvc = True
                self.rail_on = False
                log(f"BMS: LVC latched, 12V off at {self.vbat_mv} mV")
        else:
            self._below_since = None
            if self.lvc and self.vbat_mv > LVC_RECOVER_MV:
                self.lvc = False
                log(f"BMS: LVC recovered at {self.vbat_mv} mV")

    def beacon(self):
        return Frame(ID_BEACON, struct.pack(
            ">HBB", max(0, min(0xFFFF, self.vbat_mv)),
            0x01 if self.rail_on else 0x02,
            0x01 if self.lvc else 0x00))


# ================================================================= detectors ==

class Detectors:
    """C2 and C3, prototyped here before they are compiled into the RP2040.

    Both are gated by the ARM MASK carried in heartbeat byte 1 (ARCHITECTURE.md
    section D). Arming is POSITIVE: bit set = armed. Any failure -- stale
    heartbeat, DLC < 2, no heartbeat at all -- yields mask 0, i.e. DISARMED.
    A dead station is recoverable; a silently leaked flag is not.
    """

    def __init__(self, code_c2, code_c3):
        self.code_c2 = code_c2.encode()[:8].ljust(8, b"\x00")
        self.code_c3 = code_c3.encode()[:8].ljust(8, b"\x00")
        self.arm_mask = 0
        self.hb_last = None
        # C2
        self.c2_until = 0.0
        self.c2_fires = 0
        # C3
        self.c3_hits = []          # monotonic timestamps of qualifying commands
        self.c3_until = 0.0
        self.c3_fires = 0

    def on_heartbeat(self, f, now):
        self.hb_last = now
        # DLC < 2 => no flags field => disarmed, per the fail-safe direction.
        self.arm_mask = f.data[1] if len(f.data) >= 2 else 0

    def refresh(self, now):
        if self.hb_last is None or (now - self.hb_last) * 1000 > HEARTBEAT_TIMEOUT_MS:
            self.arm_mask = 0

    def armed(self, bit):
        return bool(self.arm_mask & bit)

    def on_command(self, motors, which, d, rpm, now):
        # MIRRORS TinytrekBMS.ino noteCommand() EXACTLY. If you change one, change
        # the other in the same commit -- this is the model the bench validates
        # against, so a divergence here means the suite certifies behaviour no car
        # has. The pair is now computed ONCE and shared by C2 and C3, as in the
        # firmware; it used to be computed twice from two separate lookups.
        if d == DIR_STOP:
            self.c3_hits.clear()          # an explicit stop breaks the run
            return

        other = motors["R" if which == "L" else "L"]
        paired, same_dir_pair, gap_ms = False, False, 0.0
        if other.last_cmd:
            t_other, d_other, _ = other.last_cmd
            paired = (now - t_other) <= C2_PAIR_WINDOW_S
            same_dir_pair = paired and d_other == d
            gap_ms = (now - t_other) * 1000

        # A PAIR IS CONSUMED ONCE EVALUATED, so a pair must be formed from two
        # FRESH commands. Without this the partner timestamp stays live and pairs
        # again with whatever comes next: two C1 pivot calls with opposite option
        # bytes inside the window line up at the boundary (first call's R and second
        # call's L share a direction) and fire C2 at a team that has only solved C1.
        if paired:
            motors["L"].last_cmd = None
            motors["R"].last_cmd = None

        # ---- C2: both motors commanded in the SAME dir within a short window.
        # No legitimate interface commands translation while the panel is locked;
        # the pivot always drives the two wheels in OPPOSITE directions.
        if self.armed(ARM_C2) and same_dir_pair:
            if now > self.c2_until:
                log(f"DETECT C2: both motors dir={d:#04x} within "
                    f"{gap_ms:.0f} ms -> 0x7D1")
            self.c2_until = now + C2_HOLD_S

        # ---- C3: sustained SAME-DIR commanding at an rpm that never appears in
        # legitimate locked traffic. C2's composed-replay technique CANNOT reach
        # this: composed frames are captured frames and structurally carry rpm=50.
        #
        # The same-dir requirement is not optional, and dropping it is a real trap.
        # A first cut here keyed on rpm alone, which passed the positive test
        # (forward driving trips C3) and would have failed silently in the one case
        # that matters: if the pivot rpm is ever raised off 50 -- and there is live
        # pressure to raise it, see the resonance note in ttos-dashboard.default --
        # an rpm-only rule fires 0x7D2 on the car's OWN legitimate pivot routine and
        # leaks the C3 code to anyone sniffing, before the challenge is even attempted.
        # ONLY EVALUATED PAIRS COUNT, and only they can break the run. The first
        # command of a pair is not yet evidence of anything -- it has no partner to
        # be same-direction WITH. Letting it reach the else branch below clears the
        # run on every other command, and since a pair now consumes both timestamps,
        # every second command is a first-of-pair: the hit count would never exceed
        # one and C3 could never fire at all.
        if self.armed(ARM_C3) and paired:
            if same_dir_pair and rpm != PIVOT_RPM:
                self.c3_hits.append(now)
                self.c3_hits = [t for t in self.c3_hits if now - t <= C3_WINDOW_S]
                if len(self.c3_hits) >= C3_COUNT:
                    if now > self.c3_until:
                        log(f"DETECT C3: {len(self.c3_hits)} qualifying pairs at "
                            f"rpm={rpm} (pivot is {PIVOT_RPM}) in {C3_WINDOW_S:.0f} s -> 0x7D2")
                    self.c3_until = now + C2_HOLD_S
            else:
                # "CONSECUTIVE" -- a pair that does not qualify breaks the run.
                self.c3_hits.clear()

    def due(self, now):
        """Flag frames to emit this tick. Emission repeats at 2 Hz while the
        condition holds, so a contestant who solved it without a sniffer running
        can simply do it again."""
        out = []
        if self.armed(ARM_C2) and now < self.c2_until:
            out.append(Frame(ID_FLAG_C2, self.code_c2))
        if self.armed(ARM_C3) and now < self.c3_until:
            out.append(Frame(ID_FLAG_C3, self.code_c3))
        return out


# ================================================================== emulator ==

class Emulator:
    def __init__(self, args, car):
        self.args = args
        self.car = car
        self.iface = args.iface
        did_l = int(car["dataid_l"], 16)
        did_r = int(car["dataid_r"], 16)
        self.motors = {
            "L": Motor("L", ID_MOTOR_L, did_l),
            "R": Motor("R", ID_MOTOR_R, did_r),
        }
        self.bms = BMS(args.vbat)
        self.det = Detectors(car["code_c2"], car["code_c3"])

        # wire_only=False is REQUIRED here, and it is the opposite of what every
        # assertion-side Observer wants.
        #
        # A real motor node is a separate physical device, so everything that
        # reaches it arrived over the wire by definition. This emulator is a
        # process on the same host as the test harness, so harness-injected frames
        # are tagged MSG_DONTROUTE -- and an emulated node that filters those is
        # deaf to exactly the traffic the tests use to drive it. Every positive
        # detection case failed this way on first run while the negative cases
        # "passed", which is the worst possible arrangement.
        #
        # The rule: emulated NODES hear everything (wire_only=False); test
        # OBSERVERS making claims about what crossed a bus hear only the wire
        # (wire_only=True, the default).
        #
        # Safe against self-feedback: this node transmits 0x116/0x7D1/0x7D2 and
        # handles only 0x100/0x111/0x113/0x115, so its own echo is ignored.
        self.obs = Observer(self.iface, errors=True, wire_only=False)
        self.inj = Injector(self.iface)

        # fault injection
        self.drop_pct = 0.0
        self.mute = False
        self.latency_ms = 0.0

        self.stop = threading.Event()
        self.lock = threading.Lock()
        self.rx_total = 0
        self.err_frames = 0
        self.fd_seen = 0
        self.tx_fail = 0

    # ------------------------------------------------------------ validation --
    def check_command(self, f, motor):
        """Return (steps, dir, rpm) or None.

        None means SILENT REJECTION: no response of any kind. An error reply here
        would be an oracle that shortcuts C2 -- a contestant could brute-force the
        CRC by watching for the frame that stops being rejected.
        """
        n = len(f.data)

        if self.args.protect in ("on", "both") and n == 8:
            covered = self.args.crc_covers      # 6 (spec) or 7 (recommended)
            want = f.data[7]
            # AUTOSAR E2E Profile 1 convention: Data ID prefixed, low byte first.
            got = crc8_j1850(struct.pack("<H", motor.data_id) + f.data[:covered])
            if got != want:
                motor.rejected += 1
                return None
            return (struct.unpack(">I", f.data[:4])[0], f.data[4], f.data[5])

        if self.args.protect in ("off", "both") and n >= 5:
            # Phase 0/1 unprotected form: [steps u32 BE][dir][rpm?]
            rpm = f.data[5] if n >= 6 and f.data[5] > 0 else 0
            return (struct.unpack(">I", f.data[:4])[0], f.data[4], rpm)

        motor.rejected += 1
        return None

    # -------------------------------------------------------------- rx loop ---
    def rx_loop(self):
        while not self.stop.is_set():
            for f in self.obs.collect(0.05):
                now = time.monotonic()
                with self.lock:
                    self.rx_total += 1
                    if f.err:
                        self.err_frames += 1
                        continue
                    if f.fd:
                        # A CAN FD frame reached the drive bus. On a real MCP2515
                        # node this is a form error, not data -- error-passive,
                        # then bus-off, then propulsion stops. Matrix G3.
                        self.fd_seen += 1
                        log(f"*** CAN FD FRAME ON THE DRIVE BUS: {f!r} -- real "
                            f"MCP2515 nodes would go error-passive here")
                        continue
                    if self.drop_pct and random.random() * 100 < self.drop_pct:
                        continue
                    self._handle(f, now)

    def _handle(self, f, now):
        if f.can_id == ID_HEARTBEAT:
            self.det.on_heartbeat(f, now)
            return
        if f.can_id == ID_POWER:
            if f.data:
                self.bms.on_power_cmd(f.data[0])
            return
        for which, m in self.motors.items():
            if f.can_id == m.can_id:
                parsed = self.check_command(f, m)
                if parsed is None:
                    return                      # silent
                steps, d, rpm = parsed
                m.command(steps, d, rpm or m.rpm)
                self.det.on_command(self.motors, which, d, m.rpm, now)
                return

    # ------------------------------------------------------------ tick loop ---
    def tick_loop(self):
        last = time.monotonic()
        next_beacon = last
        next_flag = last
        while not self.stop.is_set():
            time.sleep(0.01)
            now = time.monotonic()
            dt, last = now - last, now
            with self.lock:
                self.det.refresh(now)
                self.bms.tick(now)
                for m in self.motors.values():
                    m.drain(dt, self.bms.rail_on)

                if now >= next_beacon:
                    next_beacon = now + BEACON_MS / 1000.0
                    self._tx(self.bms.beacon())

                if now >= next_flag:
                    next_flag = now + 1.0 / FLAG_EMIT_HZ
                    for fr in self.det.due(now):
                        self._tx(fr)
                        if fr.can_id == ID_FLAG_C2:
                            self.det.c2_fires += 1
                        else:
                            self.det.c3_fires += 1

    def _tx(self, frame):
        if self.mute:
            return
        if self.latency_ms:
            time.sleep(self.latency_ms / 1000.0)
        try:
            self.inj.send(frame)
        except OSError as e:
            # ENOBUFS = nobody ACKed. On this project that has meant, every time,
            # that the far node is absent or the bus is not actually connected.
            self.tx_fail += 1
            if self.tx_fail in (1, 10, 100) or self.tx_fail % 1000 == 0:
                log(f"TX failed ({self.tx_fail}x): {e} -- is the DUT on this bus?")

    # ---------------------------------------------------------- control sock --
    def ctl_loop(self):
        try:
            os.unlink(CTL_SOCK)
        except FileNotFoundError:
            pass
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        srv.bind(CTL_SOCK)
        os.chmod(CTL_SOCK, 0o666)
        srv.listen(4)
        srv.settimeout(0.5)
        while not self.stop.is_set():
            try:
                conn, _ = srv.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            try:
                line = conn.recv(4096).decode().strip()
                conn.sendall((self.command(line) + "\n").encode())
            except OSError:
                pass
            finally:
                conn.close()
        try:
            os.unlink(CTL_SOCK)
        except OSError:
            pass

    def command(self, line):
        parts = line.split()
        if not parts:
            return "usage: status|drop|mute|latency|lvc|vbat|sweep|rail|reset"
        cmd, a = parts[0], parts[1:]
        with self.lock:
            try:
                if cmd == "status":
                    return self.status()
                if cmd == "drop":
                    self.drop_pct = float(a[0]); return f"drop={self.drop_pct}%"
                if cmd == "mute":
                    self.mute = a[0] == "on"; return f"mute={self.mute}"
                if cmd == "latency":
                    self.latency_ms = float(a[0]); return f"latency={self.latency_ms}ms"
                if cmd == "lvc":
                    self.bms.lvc = a[0] == "on"
                    if self.bms.lvc:
                        self.bms.rail_on = False
                    return f"lvc={self.bms.lvc}"
                if cmd == "vbat":
                    self.bms.sweep = None
                    self.bms.vbat_mv = int(a[0]); return f"vbat={self.bms.vbat_mv}mV"
                if cmd == "sweep":
                    self.bms.sweep = (int(a[0]), int(a[1]), float(a[2]),
                                      time.monotonic())
                    return f"sweep {a[0]}..{a[1]} over {a[2]}s"
                if cmd == "rail":
                    self.bms.rail_on = a[0] == "on"; return f"rail={self.bms.rail_on}"
                if cmd == "reset":
                    self.drop_pct = 0.0; self.mute = False; self.latency_ms = 0.0
                    self.bms.sweep = None; self.bms.lvc = False
                    self.bms.vbat_mv = self.args.vbat
                    self.det.c2_until = self.det.c3_until = 0.0
                    self.det.c3_hits.clear()
                    return "reset"
            except (IndexError, ValueError) as e:
                return f"error: {e}"
        return f"unknown command: {cmd}"

    def status(self):
        L, R = self.motors["L"], self.motors["R"]
        mask = self.det.arm_mask
        return (
            f"car={self.car['car_id']} iface={self.iface} protect={self.args.protect} "
            f"crc_covers=0..{self.args.crc_covers - 1}\n"
            f"rx={self.rx_total} err={self.err_frames} fd_on_drive={self.fd_seen} "
            f"tx_fail={self.tx_fail}\n"
            f"armed: C2={'Y' if mask & ARM_C2 else 'n'} C3={'Y' if mask & ARM_C3 else 'n'}"
            f" (mask={mask:#04x})  heartbeat={'fresh' if mask or self.det.hb_last else 'ABSENT'}\n"
            f"bms: vbat={self.bms.vbat_mv}mV rail={'ON' if self.bms.rail_on else 'off'} "
            f"lvc={'LATCHED' if self.bms.lvc else 'clear'}\n"
            f"L: steps={L.steps_left:.0f} dir={L.dir:#04x} rpm={L.rpm} "
            f"ok={L.accepted} rejected={L.rejected} moving={L.moving and self.bms.rail_on}\n"
            f"R: steps={R.steps_left:.0f} dir={R.dir:#04x} rpm={R.rpm} "
            f"ok={R.accepted} rejected={R.rejected} moving={R.moving and self.bms.rail_on}\n"
            f"flags emitted: 0x7D1 x{self.det.c2_fires}  0x7D2 x{self.det.c3_fires}\n"
            f"faults: drop={self.drop_pct}% mute={self.mute} latency={self.latency_ms}ms"
        )

    # ------------------------------------------------------------------ run ---
    def run(self):
        log(f"emulating car {self.car['car_id']} ({self.car['hostname']}) on {self.iface}")
        log(f"  L dataid={self.car['dataid_l']}  R dataid={self.car['dataid_r']}")
        log(f"  protect={self.args.protect}  crc covers bytes 0..{self.args.crc_covers - 1}")
        log(f"  codes: C2={self.car['code_c2']}  C3={self.car['code_c3']}")
        log(f"  control socket: {CTL_SOCK}")
        threads = [threading.Thread(target=t, daemon=True)
                   for t in (self.rx_loop, self.tick_loop, self.ctl_loop)]
        for t in threads:
            t.start()
        try:
            while True:
                time.sleep(self.args.status_every)
                if self.args.status_every > 0:
                    print(self.status(), flush=True)
        except KeyboardInterrupt:
            log("stopping")
        finally:
            self.stop.set()
            time.sleep(0.3)
            self.obs.close()
            self.inj.close()


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--car", default="01", help="car_id from fleet-table.csv (default 01)")
    p.add_argument("--fleet", default=os.path.join(here, "..", "provisioning",
                                                   "fleet-table.csv"))
    p.add_argument("--iface", default=DRIVE)
    p.add_argument("--vbat", type=int, default=12100, help="initial pack mV")
    p.add_argument("--status-every", type=float, default=0,
                   help="seconds between status dumps (0 = quiet)")
    p.add_argument("--protect", choices=("off", "on", "both"), default="both",
                   help="off = 6-byte unprotected only (what the DUT sends today); "
                        "on = 8-byte CRC-protected only (phase 3); "
                        "both = accept either, by length (default, needed during "
                        "the transition)")
    p.add_argument("--crc-covers", type=int, choices=(6, 7), default=7,
                   help="bytes covered by crc8: 7 = bytes 0..6 including the nonce "
                        "(WALKTHROUGH.md 3.5 -- extra captures do NOT narrow the CRC "
                        "model uniquely); 6 = bytes 0..5 as originally specified, "
                        "which leaves only 2 capturable inputs ever and 4 "
                        "indistinguishable CRC models")
    args = p.parse_args()

    car = load_car(args.car, os.path.abspath(args.fleet))
    Emulator(args, car).run()


if __name__ == "__main__":
    main()
