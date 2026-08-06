"""A UDS-lite client for the DIAG bus, matching the server in cmd/dashboard/uds.go.

Single-frame only -- no ISO-TP multi-frame, no flow control. CAN FD gives a 64-byte
payload, which covers every response the challenges need including a flag inside a
routine response, so the whole segmentation layer is avoidable on both sides.

    0x7DF  functional request (all ECUs)
    0x7E0  physical request (this ECU)
    0x7E8  responses

PCI is real ISO 15765-2 so stock tooling works against the same server:

    payload <= 7    [0x0L][data...]           classic single frame
    payload  > 7    [0x00][len][data...]      CAN FD escape form
"""

import time

from ttoscan import DIAG, Frame, Injector, Observer, fd_len

ID_FUNCTIONAL = 0x7DF
ID_PHYSICAL = 0x7E0
ID_RESPONSE = 0x7E8

SID_SESSION = 0x10
SID_TESTER_PRESENT = 0x3E
SID_READ_DID = 0x22
SID_ROUTINE = 0x31
SID_NEGATIVE = 0x7F
RESP_OFFSET = 0x40

SESSION_DEFAULT = 0x01
SESSION_EXTENDED = 0x03

RC_START, RC_STOP, RC_RESULTS = 0x01, 0x02, 0x03

RID_PIVOT = 0x0201
RID_BRIDGE = 0x0202
RID_SELFTEST = 0x0203

DID_VIN = 0xF190
DID_ECU_SERIAL = 0xF18C

PIVOT_CW, PIVOT_CCW = 0x01, 0x02

NRC = {
    0x11: "serviceNotSupported",
    0x12: "subFunctionNotSupported",
    0x13: "incorrectMessageLengthOrInvalidFormat",
    0x22: "conditionsNotCorrect",
    0x31: "requestOutOfRange",
    0x33: "securityAccessDenied",
    0x78: "responsePending",
    0x7E: "subFunctionNotSupportedInActiveSession",
    0x7F: "serviceNotSupportedInActiveSession",
}


class NegativeResponse(Exception):
    def __init__(self, sid, nrc):
        self.sid, self.nrc = sid, nrc
        super().__init__(f"NRC {nrc:#04x} ({NRC.get(nrc, 'unknown')}) "
                         f"to service {sid:#04x}")


def pack_single_frame(payload):
    """Wrap a UDS payload in an ISO 15765-2 single frame."""
    if len(payload) <= 7:
        return bytes([len(payload)]) + payload, False
    if len(payload) > 62:
        raise ValueError("payload too long for a single FD frame")
    body = bytes([0x00, len(payload)]) + payload
    return body.ljust(fd_len(len(body)), b"\x00"), True


def unpack_single_frame(data):
    """Return the UDS payload, or None if this is not a single frame we handle."""
    if not data:
        return None
    pci = data[0]
    if pci & 0xF0 == 0x00:
        if pci == 0x00:                      # FD escape form
            if len(data) < 2:
                return None
            n = data[1]
            return data[2:2 + n] if n and len(data) >= 2 + n else None
        n = pci & 0x0F                       # classic single frame
        return data[1:1 + n] if n and len(data) >= 1 + n else None
    return None


class Tester:
    """One diagnostic tester on the DIAG bus.

    The observer is wire-only, so it cannot mistake our own request for the ECU's
    response -- both ride the same interface, which is exactly the case the
    loopback trap ruins.
    """

    def __init__(self, iface=DIAG, timeout=1.0):
        self.iface = iface
        self.timeout = timeout
        self.obs = Observer(iface)
        self.inj = Injector(iface)

    def close(self):
        self.obs.close()
        self.inj.close()

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.close()

    def request(self, payload, functional=False, timeout=None, raise_negative=True):
        """Send a UDS request, return the response payload.

        Returns None on timeout -- which is a legitimate outcome for a suppressed
        positive response, not necessarily a fault.
        """
        body, fd = pack_single_frame(bytes(payload))
        self.obs.drain()
        self.inj.send(Frame(ID_FUNCTIONAL if functional else ID_PHYSICAL,
                            body, fd=fd, brs=fd))
        deadline = time.monotonic() + (timeout if timeout is not None else self.timeout)
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            f = self.obs.wait_for(remaining, lambda fr: fr.can_id == ID_RESPONSE)
            if f is None:
                return None
            resp = unpack_single_frame(f.data)
            if resp is None:
                continue
            # 0x78 responsePending: the ECU is asking for more time. This server
            # never sends it, but a client that treats it as a final answer would
            # be wrong against any real ECU, so handle it rather than bake in an
            # assumption about our own implementation.
            if len(resp) >= 3 and resp[0] == SID_NEGATIVE and resp[2] == 0x78:
                deadline = time.monotonic() + (timeout if timeout is not None
                                               else self.timeout)
                continue
            if raise_negative and len(resp) >= 3 and resp[0] == SID_NEGATIVE:
                raise NegativeResponse(resp[1], resp[2])
            return resp

    # ---- services ---------------------------------------------------------
    def session(self, s, **kw):
        return self.request([SID_SESSION, s], **kw)

    def tester_present(self, suppress=False, **kw):
        # Bit 7 of the sub-function is suppressPosRspMsgIndication: the ECU acts
        # on the request and says nothing, so a timeout is the expected result.
        return self.request([SID_TESTER_PRESENT, 0x80 if suppress else 0x00], **kw)

    def read_did(self, did, **kw):
        return self.request([SID_READ_DID, did >> 8, did & 0xFF], **kw)

    def routine(self, rid, sub=RC_START, option=None, **kw):
        req = [SID_ROUTINE, sub, rid >> 8, rid & 0xFF]
        if option is not None:
            req.append(option)
        return self.request(req, **kw)

    def pivot(self, direction=PIVOT_CW, **kw):
        """Run the C1 routine. Returns the routineStatusRecord.

        That record is the C1 flag in submission form -- b"FLAG{XXXXXXXX}" -- not
        the bare eight characters. 4 + 14 = 18 bytes is not a discrete CAN FD
        length so the controller pads to 20; the payload ends at the brace, so the
        pad strips unambiguously.
        """
        r = self.routine(RID_PIVOT, RC_START, direction, **kw)
        return r[4:].rstrip(b"\x00") if r and len(r) > 4 else None
