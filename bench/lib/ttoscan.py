"""SocketCAN primitives for the TTOS CTF bench rig.

The one thing this module exists to get right is the LOOPBACK TRAP.

SocketCAN delivers a frame transmitted by one local socket to every other local
socket bound to the same interface. An observer on the bench therefore sees the
bench's own injected frames by default, indistinguishable from frames that came
off the wire. Every same-bus assertion written against a naive observer is
worthless -- inject 0x111 on DIAG, observe 0x111 on DIAG, conclude the gateway
bridged it, when in fact it never left the host.

THE DOC'S PRESCRIBED FIX DOES NOT WORK. The bench doc says to clear
CAN_RAW_LOOPBACK on the observing socket and calls that the cleaner of its two
options. Measured on this rig 2026-08-03, it has no effect:

    LOOPBACK=0 on the RECEIVER -> receiver still saw the injected frame
    LOOPBACK=0 on the SENDER   -> receiver did not

CAN_RAW_LOOPBACK is a property of the TRANSMITTING socket: it controls whether
frames THAT SOCKET sends are echoed to other local sockets. Clearing it on a
receive-only socket suppresses the loopback of the frames it never sends, which
is to say nothing at all. An observer built to the doc is silently unprotected,
and -- this is the nasty part -- it still passes the doc's own self-test, because
that test only asks whether the injected frame was absent, and a deaf socket
answers yes.

What Observer() actually does is filter on the receive side using msg_flags. The
kernel tags every frame that originated from a local socket with MSG_DONTROUTE
(and additionally MSG_CONFIRM when this same socket sent it); frames off the wire
carry neither. Verified on this rig: locally injected frames arrived with
msg_flags=0x4, the DUT's heartbeat with msg_flags=0x0.

That is strictly better than either option the doc offers. Sender-side loopback
suppression only covers injectors that opt in, so cansend, candump-driven
scripts and the panel would all still leak; frame bookkeeping (the doc's option
2) needs every transmitter to report to a central ledger. MSG_DONTROUTE needs
no cooperation from anyone: it is the kernel's own statement about where the
frame came from.

Injector() is left at default loopback, so candump and other human-facing tools
still show bench traffic normally.

Cross-bus assertions -- inject on DIAG, observe on DRIVE -- are immune to this,
because the interfaces differ. Same-bus assertions are not. Do not use a bare
socket for either; the asymmetry is exactly the kind of thing that is true today
and forgotten in three weeks.

harness-selftest.py proves the isolation empirically, with a positive control.
Run it before trusting any result from this module.
"""

import os
import select
import socket
import struct
import time

# ---------------------------------------------------------------- constants ---

CAN_SFF_MASK = 0x000007FF
CAN_EFF_MASK = 0x1FFFFFFF
CAN_EFF_FLAG = 0x80000000
CAN_RTR_FLAG = 0x40000000
CAN_ERR_FLAG = 0x20000000

CANFD_BRS = 0x01  # bit rate switch
CANFD_ESI = 0x02  # error state indicator

SOL_CAN_RAW = getattr(socket, "SOL_CAN_RAW", 101)
CAN_RAW_FILTER = getattr(socket, "CAN_RAW_FILTER", 1)
CAN_RAW_ERR_FILTER = getattr(socket, "CAN_RAW_ERR_FILTER", 2)
CAN_RAW_LOOPBACK = getattr(socket, "CAN_RAW_LOOPBACK", 3)
CAN_RAW_RECV_OWN_MSGS = getattr(socket, "CAN_RAW_RECV_OWN_MSGS", 4)
CAN_RAW_FD_FRAMES = getattr(socket, "CAN_RAW_FD_FRAMES", 5)

# Set by the kernel in recvmsg msg_flags when the frame came from a local socket
# rather than off the wire. This is the discriminator the whole harness rests on.
MSG_DONTROUTE = getattr(socket, "MSG_DONTROUTE", 0x04)

CAN_FRAME_FMT = "=IB3x8s"     # 16 bytes -- classic
CANFD_FRAME_FMT = "=IBBBB64s"  # 72 bytes -- FD
CAN_FRAME_SZ = struct.calcsize(CAN_FRAME_FMT)
CANFD_FRAME_SZ = struct.calcsize(CANFD_FRAME_FMT)

# CAN FD payloads are quantised: any length above 8 rounds up to one of these.
FD_LENGTHS = (0, 1, 2, 3, 4, 5, 6, 7, 8, 12, 16, 20, 24, 32, 48, 64)

# Role names. The bench pins its adapters to these via udev; see
# bench/udev/99-ttos-bench-can.rules for why they are roles and not can0/can1.
DRIVE = os.environ.get("TTOS_BENCH_DRIVE_IF", "ttdrive")
DIAG = os.environ.get("TTOS_BENCH_DIAG_IF", "ttdiag")


def fd_len(n):
    """Round a payload length up to the next legal CAN FD DLC."""
    for q in FD_LENGTHS:
        if n <= q:
            return q
    raise ValueError(f"payload of {n} bytes exceeds the 64-byte CAN FD maximum")


# ---------------------------------------------------------------- frame -------

class Frame:
    """One CAN frame, classic or FD, as seen or as sent."""

    __slots__ = ("can_id", "data", "fd", "brs", "esi", "rtr", "eff", "err",
                 "iface", "ts")

    def __init__(self, can_id, data=b"", fd=False, brs=False, eff=False,
                 rtr=False, err=False, esi=False, iface=None, ts=None):
        self.can_id = can_id
        self.data = bytes(data)
        self.fd = fd
        self.brs = brs
        self.esi = esi
        self.rtr = rtr
        self.eff = eff
        self.err = err
        self.iface = iface
        self.ts = ts

    def __repr__(self):
        kind = "FD" if self.fd else "  "
        tag = " ERR" if self.err else ""
        return (f"<{self.iface or '?'} {self.can_id:03X}{tag} [{len(self.data)}]{kind} "
                f"{self.data.hex(' ').upper()}>")

    def pack(self):
        cid = self.can_id
        if self.eff:
            cid = (cid & CAN_EFF_MASK) | CAN_EFF_FLAG
        else:
            cid &= CAN_SFF_MASK
        if self.rtr:
            cid |= CAN_RTR_FLAG
        if self.fd:
            n = fd_len(len(self.data))
            flags = CANFD_BRS if self.brs else 0
            return struct.pack(CANFD_FRAME_FMT, cid, n, flags, 0, 0,
                               self.data.ljust(64, b"\x00"))
        if len(self.data) > 8:
            raise ValueError("classic CAN payload cannot exceed 8 bytes; set fd=True")
        return struct.pack(CAN_FRAME_FMT, cid, len(self.data),
                           self.data.ljust(8, b"\x00"))

    @classmethod
    def unpack(cls, buf, iface=None, ts=None):
        if len(buf) >= CANFD_FRAME_SZ:
            cid, n, flags, _, _, data = struct.unpack(CANFD_FRAME_FMT, buf[:CANFD_FRAME_SZ])
            fd, brs, esi = True, bool(flags & CANFD_BRS), bool(flags & CANFD_ESI)
        else:
            cid, n, data = struct.unpack(CAN_FRAME_FMT, buf[:CAN_FRAME_SZ])
            fd = brs = esi = False
        eff = bool(cid & CAN_EFF_FLAG)
        return cls(
            can_id=(cid & CAN_EFF_MASK) if eff else (cid & CAN_SFF_MASK),
            data=data[:n], fd=fd, brs=brs, esi=esi,
            rtr=bool(cid & CAN_RTR_FLAG), eff=eff, err=bool(cid & CAN_ERR_FLAG),
            iface=iface, ts=ts,
        )


# ---------------------------------------------------------------- sockets -----

def _bind(iface, fd_frames=True):
    s = socket.socket(socket.AF_CAN, socket.SOCK_RAW, socket.CAN_RAW)
    if fd_frames:
        # Without this the kernel refuses to deliver FD frames to us at all --
        # they are silently dropped, which for an observer means "no FD frame
        # arrived" is indistinguishable from "I cannot see FD frames". Always on.
        try:
            s.setsockopt(SOL_CAN_RAW, CAN_RAW_FD_FRAMES, 1)
        except OSError:
            pass  # interface is classic-only; classic frames still arrive
    s.bind((iface,))
    return s


class Observer:
    """A receive-only view of one bus that CANNOT see bench-injected frames.

    Frames tagged MSG_DONTROUTE by the kernel came from a local socket, not the
    wire, and are dropped. That is the property every negative assertion in the
    test matrix depends on. See the module docstring for why the obvious
    CAN_RAW_LOOPBACK approach does not achieve it.

    wire_only=False disables the filter. Only useful for debugging the harness
    itself -- never for an assertion.
    """

    def __init__(self, iface, can_filter=None, errors=False, wire_only=True):
        self.iface = iface
        self.wire_only = wire_only
        self.sock = _bind(iface)
        if errors:
            # Deliver error frames too. On a classic-configured DRIVE adapter a
            # stray CAN FD frame shows up as a form error rather than as data,
            # so G3 needs these to see anything at all.
            self.sock.setsockopt(SOL_CAN_RAW, CAN_RAW_ERR_FILTER, 0x1FFFFFFF)
        if can_filter:
            self.set_filter(can_filter)
        self.sock.setblocking(False)

    def set_filter(self, pairs):
        """pairs: [(can_id, mask), ...]. Empty list = receive nothing."""
        blob = b"".join(struct.pack("=II", i, m) for i, m in pairs)
        self.sock.setsockopt(SOL_CAN_RAW, CAN_RAW_FILTER, blob)

    def drain(self):
        """Discard anything already queued. Call before an assertion window."""
        n = 0
        while True:
            r, _, _ = select.select([self.sock], [], [], 0)
            if not r:
                return n
            try:
                self.sock.recv(CANFD_FRAME_SZ)
                n += 1
            except OSError:
                return n

    def collect(self, seconds, match=None, stop_after=None):
        """Gather frames for a fixed window. Returns a list.

        match:      optional predicate(Frame) -> bool
        stop_after: return early once this many matching frames are in hand.
                    Leave it None for negative assertions -- returning early on
                    a count you expect to be zero is not a thing that can happen,
                    and the full window is what makes "never appeared" mean
                    something.
        """
        out = []
        deadline = time.monotonic() + seconds
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return out
            r, _, _ = select.select([self.sock], [], [], remaining)
            if not r:
                return out
            try:
                buf, _anc, flags, addr = self.sock.recvmsg(CANFD_FRAME_SZ)
            except OSError:
                continue
            # Locally generated: this frame never touched the wire. Dropping it
            # here is what makes "it never appeared on this bus" a real claim.
            if self.wire_only and (flags & MSG_DONTROUTE):
                continue
            f = Frame.unpack(buf, iface=addr[0] if addr else self.iface,
                             ts=time.monotonic())
            if match and not match(f):
                continue
            out.append(f)
            if stop_after and len(out) >= stop_after:
                return out

    def wait_for(self, seconds, match):
        """Return the first matching Frame within the window, else None."""
        got = self.collect(seconds, match=match, stop_after=1)
        return got[0] if got else None

    def close(self):
        self.sock.close()

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()


class Injector:
    """A transmit socket. Keeps default loopback so normal tools are unaffected."""

    def __init__(self, iface):
        self.iface = iface
        self.sock = _bind(iface)

    def send(self, frame):
        """Transmit. Raises OSError(ENOBUFS) when the bus has no other node.

        ENOBUFS here means NO ACK -- the controller is retransmitting a frame
        nobody acknowledged until the queue backs up. On this project that has
        meant, every single time, that the far node is absent or the bus is not
        actually connected. It is not a software bug; do not retry around it.
        """
        self.sock.send(frame.pack())

    def send_raw(self, can_id, data, **kw):
        self.send(Frame(can_id, data, **kw))

    def close(self):
        self.sock.close()

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()


# ---------------------------------------------------------------- crc ---------

def crc8_j1850(data, init=0xFF):
    """CRC-8/SAE-J1850: poly 0x1D, init 0xFF, xorout 0xFF.

    This is the AUTOSAR E2E Profile 1 CRC the motor nodes use over drive-command
    bytes 0..5 plus the hidden per-motor Data ID.

    Note for challenge work: this CRC is linear over GF(2), so for a FIXED-length
    Data ID, keying it is equivalent to a constant XOR offset on the result. That
    is why capturing more valid frames does not narrow the Data ID search -- one
    capture leaves 256 candidates and so does three. See CHALLENGE-PLAN.md I.2.
    """
    crc = init
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = ((crc << 1) ^ 0x1D) & 0xFF if crc & 0x80 else (crc << 1) & 0xFF
    return crc ^ 0xFF


def drive_crc(payload6, data_id):
    """CRC for a drive command: 6 payload bytes keyed by a 16-bit Data ID.

    Byte order matters and is NOT arbitrary -- it must match the firmware. The
    firmware appends the Data ID after the payload (big-endian). If you change
    this, the emulator will silently reject every real frame, which looks exactly
    like a bus fault.
    """
    if len(payload6) != 6:
        raise ValueError("drive command payload is 6 bytes (steps u32 BE, dir, rpm)")
    return crc8_j1850(bytes(payload6) + struct.pack(">H", data_id))
