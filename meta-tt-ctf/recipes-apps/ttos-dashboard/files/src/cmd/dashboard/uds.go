package main

// UDS-lite diagnostic server on the DIAG bus (can0).
//
// Single-frame only: no ISO-TP multi-frame, no flow control. CAN FD gives a 64-byte
// payload, which is enough for every response the challenges need -- including a
// flag inside a routine response -- so the whole segmentation layer is avoidable.
//
// Requests are accepted on 0x7DF (functional) and 0x7E0 (physical); responses go out
// on 0x7E8. Frames still carry a real ISO 15765-2 single-frame PCI so stock tooling
// (python-can-isotp, CaringCaribou, a commercial tester) works unmodified:
//
//	payload <= 7    [0x0L][data...]                 classic single frame
//	payload  > 7    [0x00][len][data...]            CAN FD escape form
//
// The diagnostic bus is SILENT until a contestant plugs into the side tap, and
// transmits fail with ENOBUFS until they do -- a lone node gets no acknowledgement.
// That is authentic for a diagnostic port. This server only ever transmits in
// response to a request, so an unattached bus costs nothing: no unsolicited traffic,
// no spinning, no log flood.

import (
	"errors"
	"sync"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

var errNoBus = errors.New("bus not open")

const (
	idDiagFunctional = 0x7DF // functional request (all ECUs)
	idDiagPhysical   = 0x7E0 // physical request (this ECU)
	idDiagResponse   = 0x7E8 // this ECU's response
)

// Services.
const (
	sidSessionControl = 0x10
	sidTesterPresent  = 0x3E
	sidReadDataByID   = 0x22
	sidRoutineControl = 0x31
	sidNegative       = 0x7F
	respOffset        = 0x40 // positive response SID = request SID + 0x40
)

// Negative response codes.
const (
	nrcSubFunctionNotSupported     = 0x12
	nrcIncorrectLength             = 0x13
	nrcConditionsNotCorrect        = 0x22
	nrcRequestOutOfRange           = 0x31
	nrcSecurityAccessDenied        = 0x33
	nrcServiceNotSupported         = 0x11
	nrcServiceNotSupportedInActive = 0x7F
)

// Diagnostic sessions.
const (
	sessionDefault  = 0x01
	sessionExtended = 0x03
)

// Data identifiers.
const (
	didVIN       = 0xF190 // 17 chars, default session -- readable by design
	didECUSerial = 0xF18C // extended session only; this is what ties C3 to the C2 skill
)

// s3Timeout is the UDS S3 session timer: an extended session falls back to default
// after this long with no request. 0x3E TesterPresent is how a contestant holds it
// open, which is what puts authentic scripting pressure on the C2 timing problem.
const s3Timeout = 5000 * time.Millisecond

// udsSession is the server's single session slot. A real ECU tracks one session per
// tester; there is only ever one tap here.
var udsSession struct {
	sync.Mutex
	active   byte
	lastSeen time.Time
}

// current returns the active session, expiring it first if S3 has elapsed.
func currentSession() byte {
	udsSession.Lock()
	defer udsSession.Unlock()
	if udsSession.active != sessionDefault && time.Since(udsSession.lastSeen) > s3Timeout {
		udsSession.active = sessionDefault
	}
	return udsSession.active
}

func touchSession() {
	udsSession.Lock()
	udsSession.lastSeen = time.Now()
	udsSession.Unlock()
}

func setSession(s byte) {
	udsSession.Lock()
	udsSession.active = s
	udsSession.lastSeen = time.Now()
	udsSession.Unlock()
}

// ---- ISO-TP single-frame codec -------------------------------------------

// unpackSingleFrame extracts a service payload from a single frame, classic or the
// CAN FD escape form. Returns nil for anything that is not a well-formed SF --
// multi-frame PCIs (FF/CF/FC) are ignored rather than answered, since this server
// deliberately implements no segmentation.
func unpackSingleFrame(d []byte) []byte {
	if len(d) < 2 {
		return nil
	}
	if d[0]>>4 != 0 { // not a single frame
		return nil
	}
	n := int(d[0] & 0x0F)
	if n == 0 { // FD escape: length in byte 1
		n = int(d[1])
		if n < 8 || 2+n > len(d) {
			return nil
		}
		return d[2 : 2+n]
	}
	if n > 7 || 1+n > len(d) {
		return nil
	}
	return d[1 : 1+n]
}

// fdLengths are the only payload sizes CAN FD can carry. The kernel rejects
// anything else with EINVAL, so responses are padded up to the next valid size.
var fdLengths = []int{8, 12, 16, 20, 24, 32, 48, 64}

func fdLen(n int) int {
	for _, l := range fdLengths {
		if n <= l {
			return l
		}
	}
	return 64
}

// packSingleFrame wraps a service payload in a single frame, padding to a legal
// length. Pad byte is 0x00 and fixed, so responses are byte-reproducible -- which
// matters when a contestant is diffing captures.
func packSingleFrame(payload []byte) ([]byte, bool) {
	if len(payload) <= 7 {
		out := make([]byte, 8)
		out[0] = byte(len(payload))
		copy(out[1:], payload)
		return out, true
	}
	if len(payload) > 62 { // 2 PCI bytes + payload must fit 64
		return nil, false
	}
	out := make([]byte, fdLen(len(payload)+2))
	out[0] = 0x00
	out[1] = byte(len(payload))
	copy(out[2:], payload)
	return out, true
}

// ---- server ---------------------------------------------------------------

var udsConn struct {
	sync.Mutex
	conn *canbus.Conn
}

// udsServe binds the DIAG bus and answers diagnostic requests forever.
func udsServe(iface string) {
	setSession(sessionDefault)
	for {
		c, err := canbus.Open(iface)
		if err != nil {
			logf("warn", "UDS: open %s failed (%v); retrying in 3s", iface, err)
			time.Sleep(3 * time.Second)
			continue
		}
		udsConn.Lock()
		udsConn.conn = c
		udsConn.Unlock()
		logf("info", "UDS server listening on DIAG bus %s (0x7DF/0x7E0 -> 0x7E8)", iface)

		for {
			f, err := c.ReadFrame()
			if err != nil {
				logf("warn", "UDS: read error on %s (%v); reopening", iface, err)
				break
			}
			if f.ID != idDiagFunctional && f.ID != idDiagPhysical {
				continue
			}
			if f.Err || f.RTR || f.Ext {
				continue
			}
			req := unpackSingleFrame(f.Data)
			if len(req) == 0 {
				continue
			}
			if resp := udsDispatch(req); resp != nil {
				udsRespond(c, resp)
			}
		}

		udsConn.Lock()
		udsConn.conn = nil
		udsConn.Unlock()
		c.Close()
		time.Sleep(time.Second)
	}
}

var udsTXWarn time.Time

func udsRespond(c *canbus.Conn, payload []byte) {
	out, ok := packSingleFrame(payload)
	if !ok {
		logf("error", "UDS: response too long for a single frame (%d bytes)", len(payload))
		return
	}
	// The DIAG bus is FD and the placard requires an FD adapter, so responses go
	// out as FD frames uniformly. BRS is left off: the data phase gains nothing at
	// these payload sizes and not every loaner adapter is happy with it.
	err := c.Send(canbus.Frame{ID: idDiagResponse, FD: true, Data: out})
	if err != nil && time.Since(udsTXWarn) > 30*time.Second {
		// Rate-limited: with no tester attached every send fails ENOBUFS (no ACK),
		// and this must not become a log flood.
		udsTXWarn = time.Now()
		logf("warn", "UDS: response TX failed (%v) -- is anything attached to the tap?", err)
	}
}

// negative builds a negative response.
func negative(sid, nrc byte) []byte { return []byte{sidNegative, sid, nrc} }

// udsDispatch handles one request and returns the response payload, or nil when the
// request must be answered with silence.
func udsDispatch(req []byte) []byte {
	sid := req[0]

	// ORDER MATTERS. Expire an idle session FIRST, then refresh the timer.
	// Refreshing first would renew a session that S3 had already run out on, so a
	// request arriving after the timeout would be served in the old session -- the
	// gate on 0xF18C would never actually close, and C3 would stop requiring the
	// session work from C2.
	currentSession()

	// Any valid request then keeps the session alive, exactly like TesterPresent.
	touchSession()

	switch sid {
	case sidSessionControl:
		return udsSessionControl(req)
	case sidTesterPresent:
		return udsTesterPresent(req)
	case sidReadDataByID:
		return udsReadDataByID(req)
	case sidRoutineControl:
		return udsRoutineControl(req)
	default:
		return negative(sid, nrcServiceNotSupported)
	}
}

// subFunc splits a sub-function byte into its value and the
// suppressPosRspMsgIndication bit (0x80), which tells the server to stay silent on
// success. Stock testers set it for TesterPresent; honouring it keeps them quiet.
func subFunc(b byte) (val byte, suppress bool) {
	return b & 0x7F, b&0x80 != 0
}

func udsSessionControl(req []byte) []byte {
	if len(req) != 2 {
		return negative(sidSessionControl, nrcIncorrectLength)
	}
	sub, suppress := subFunc(req[1])
	switch sub {
	case sessionDefault, sessionExtended:
		setSession(sub)
	default:
		return negative(sidSessionControl, nrcSubFunctionNotSupported)
	}
	if suppress {
		return nil
	}
	// 50 <session> P2(50ms) P2*(5000ms in 10ms units) -- the standard timing block.
	return []byte{sidSessionControl + respOffset, sub, 0x00, 0x32, 0x01, 0xF4}
}

func udsTesterPresent(req []byte) []byte {
	if len(req) != 2 {
		return negative(sidTesterPresent, nrcIncorrectLength)
	}
	sub, suppress := subFunc(req[1])
	if sub != 0x00 {
		return negative(sidTesterPresent, nrcSubFunctionNotSupported)
	}
	if suppress {
		return nil
	}
	return []byte{sidTesterPresent + respOffset, 0x00}
}

func udsReadDataByID(req []byte) []byte {
	if len(req) != 3 {
		return negative(sidReadDataByID, nrcIncorrectLength)
	}
	did := uint16(req[1])<<8 | uint16(req[2])

	switch did {
	case didVIN:
		if ident.VIN == "" {
			return negative(sidReadDataByID, nrcConditionsNotCorrect)
		}
		return append([]byte{sidReadDataByID + respOffset, req[1], req[2]}, []byte(ident.VIN)...)

	case didECUSerial:
		// Extended session only. This is the gate that makes C3 require the session
		// work from C2 rather than merely follow it.
		//
		// NRC 0x7F (serviceNotSupportedInActiveSession) rather than the 0x31 a
		// production ECU would more commonly return for a session-gated DID: 0x7F
		// names its own solution, and it matches the NRC the self-test routine
		// returns for the same reason. One consistent, learnable hint beats strict
		// convention on a teaching board.
		if currentSession() != sessionExtended {
			return negative(sidReadDataByID, nrcServiceNotSupportedInActive)
		}
		if ident.ECUSerial == "" {
			return negative(sidReadDataByID, nrcConditionsNotCorrect)
		}
		return append([]byte{sidReadDataByID + respOffset, req[1], req[2]}, []byte(ident.ECUSerial)...)
	}
	return negative(sidReadDataByID, nrcRequestOutOfRange)
}

// udsRoutineControl currently supports no routines. The pivot routine (C1) lands in
// Phase 2 and the decoy plus self-test/bridge window (C2) in Phase 3; both hang off
// this dispatch. Unknown routine identifiers get requestOutOfRange, which is what a
// real ECU returns and what makes a scripted RID sweep a legitimate discovery path.
func udsRoutineControl(req []byte) []byte {
	if len(req) < 4 {
		return negative(sidRoutineControl, nrcIncorrectLength)
	}
	sub, _ := subFunc(req[1])
	switch sub {
	case 0x01, 0x02, 0x03: // start / stop / requestResults
	default:
		return negative(sidRoutineControl, nrcSubFunctionNotSupported)
	}
	return negative(sidRoutineControl, nrcRequestOutOfRange)
}
