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
	// 0x7E subFunctionNotSupportedInActiveSession -- the self-test's answer in the
	// default session. Distinct from 0x7F above, which gates DID 0xF18C. Both say
	// "wrong session", which is the point: the same lesson is taught twice, once
	// on a read and once on a routine.
	nrcSubFuncNotSupportedInActive = 0x7E
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

	// didSnapshot returns the last drive command sent to each motor, protection
	// bytes intact. 0xF1A0 is in the manufacturer-specific identification block, so
	// the same 0xF1xx sweep that finds the VIN and the serial finds this too --
	// deliberate. The puzzle is the composition, not the search.
	//
	// THIS IS THE SANCTIONED CORPUS SOURCE. It exists precisely so the gateway does
	// not have to forward drive traffic outbound: a contestant who could sniff
	// 0x111/0x113 passively would get the same corpus for free, and C2 would
	// collapse from "interact with the car, then compose" into "watch and replay".
	didSnapshot = 0xF1A0
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

	case didSnapshot:
		// Readable in the DEFAULT session, deliberately. C2's difficulty is the
		// window and the composition; making the corpus itself session-gated would
		// add a second lock to the same door and teach nothing new.
		l, r, ok := readSnapshot()
		if !ok {
			// Nothing has been commanded yet. conditionsNotCorrect rather than an
			// empty record: a zero-filled snapshot would look like a valid corpus
			// of all-zero frames and send a contestant off composing garbage.
			return negative(sidReadDataByID, nrcConditionsNotCorrect)
		}
		// Self-describing: each entry is [id hi][id lo][8 protected bytes], so a
		// contestant does not have to guess which record belongs to which motor.
		// 3 + 2*10 = 23 bytes, which is why this rides a CAN FD frame.
		out := []byte{sidReadDataByID + respOffset, req[1], req[2]}
		for _, f := range []canbus.Frame{l, r} {
			out = append(out, byte(f.ID>>8), byte(f.ID))
			out = append(out, f.Data...)
		}
		return out
	}
	return negative(sidReadDataByID, nrcRequestOutOfRange)
}

// Routine identifiers.
//
// A CONTIGUOUS, PLAUSIBLE BLOCK, deliberately. RoutineControl identifiers are
// 16 bit, so hiding them would turn the challenge into a 65,536-request scripted
// sweep rather than a puzzle. Finding 0x0201 makes 0x0202 and 0x0203 the obvious
// next probes, which is the intended discovery path -- the puzzle is the window
// and the composition, not the search.
const (
	ridPivot    = 0x0201 // C1: rotate in place, flag in the positive response
	ridBridge   = 0x0202 // Phase 3 decoy -- always 0x33, even in extended session
	ridSelfTest = 0x0203 // Phase 3 -- the real door; opens the inbound bridge 5 s
)

// Routine control sub-functions.
const (
	rcStart          = 0x01
	rcStop           = 0x02
	rcRequestResults = 0x03
)

// udsRoutineControl dispatches the diagnostic routines. The decoy and the
// self-test/bridge window (C2) land in Phase 3 and hang off the same switch.
// Unknown routine identifiers get requestOutOfRange, which is what a real ECU
// returns and what makes a scripted RID sweep a legitimate discovery path.
func udsRoutineControl(req []byte) []byte {
	if len(req) < 4 {
		return negative(sidRoutineControl, nrcIncorrectLength)
	}
	sub, _ := subFunc(req[1])
	switch sub {
	case rcStart, rcStop, rcRequestResults:
	default:
		return negative(sidRoutineControl, nrcSubFunctionNotSupported)
	}
	rid := uint16(req[2])<<8 | uint16(req[3])

	switch rid {
	case ridPivot:
		return udsPivot(req, sub, rid)
	case ridBridge:
		return udsBridgeDecoy(sub, rid)
	case ridSelfTest:
		return udsSelfTest(sub, rid)
	}
	return negative(sidRoutineControl, nrcRequestOutOfRange)
}

// udsBridgeDecoy is the routine that is obviously named "enable diagnostic
// bridging" and NEVER works. It answers 0x33 securityAccessDenied in every session,
// including extended.
//
// It is not padding. The pair of routines is the hint structure: this one says
// "this door is real and permanently locked", the self-test says "wrong session".
// A contestant who finds only the decoy learns the feature exists; a contestant who
// finds both learns that the way in is not the door marked ENTRANCE. Making this
// one openable under any condition -- including a future "just for debugging"
// toggle -- removes the entire C2 puzzle.
func udsBridgeDecoy(sub byte, rid uint16) []byte {
	logf("cmd", "decoy bridging routine invoked (always refused)")
	return negative(sidRoutineControl, nrcSecurityAccessDenied)
}

// udsSelfTest is the actual door: an actuator self-test that legitimately needs the
// inbound path open while it runs, and so opens it for 5 s.
//
// EXTENDED SESSION ONLY. In the default session it returns NRC 0x7E,
// subFunctionNotSupportedInActiveSession -- an NRC that names its own solution.
// Reading an NRC and acting on it is the most transferable UDS skill on the board,
// and it costs four lines of server state.
func udsSelfTest(sub byte, rid uint16) []byte {
	if sub != rcStart {
		return negative(sidRoutineControl, nrcSubFunctionNotSupported)
	}
	if currentSession() != sessionExtended {
		return negative(sidRoutineControl, nrcSubFuncNotSupportedInActive)
	}
	until := openBridge()
	logf("cmd", "self-test routine: inbound DIAG->DRIVE bridge open for %s (until %s)",
		bridgeWindow, until.Format("15:04:05.000"))
	// routineStatusRecord: window length in milliseconds, big-endian. A tester
	// needs to know how long it has, and it is not a secret -- the panel's bridging
	// indicator shows the same thing.
	ms := uint16(bridgeWindow / time.Millisecond)
	return []byte{sidRoutineControl + respOffset, sub, byte(rid >> 8), byte(rid),
		byte(ms >> 8), byte(ms)}
}

// Pivot direction option byte.
const (
	pivotCW  = 0x01 // clockwise seen from above: left wheel forward, right reverse
	pivotCCW = 0x02
)

// udsPivot runs the C1 routine: rotate in place through a fixed arc, then return
// the car's C1 unlock code in the positive response.
//
// ONE COMMAND PER WHEEL, ALWAYS -- this is a hard constraint, not an efficiency
// choice. STEPS_MAX on the motor node is 250 and `steps` is ADDED to a drain
// buffer, so a multi-frame pivot would force the Phase 3 snapshot DID to choose
// which frame to return, and would turn C2's composition step into recombining
// chunk sequences instead of pairing two clean frames. 102 steps leaves 2.45x
// headroom. Scale is 0.4411 deg/step, so anything up to 110 deg fits in one command.
func udsPivot(req []byte, sub byte, rid uint16) []byte {
	if sub != rcStart {
		// stop/requestResults on a routine that completes synchronously has
		// nothing to report.
		return negative(sidRoutineControl, nrcSubFunctionNotSupported)
	}
	if len(req) < 5 {
		return negative(sidRoutineControl, nrcIncorrectLength)
	}

	var ld, rd byte
	switch req[4] {
	case pivotCW:
		ld, rd = 0x01, 0x02
	case pivotCCW:
		ld, rd = 0x02, 0x01
	default:
		return negative(sidRoutineControl, nrcRequestOutOfRange)
	}

	if !ident.complete {
		// No identity means no code to hand back. Fail loudly rather than
		// returning a positive response with an empty flag, which would look
		// like a solved challenge that awards nothing.
		logf("error", "pivot routine invoked but CTF identity is incomplete -- no C1 code to return")
		return negative(sidRoutineControl, nrcConditionsNotCorrect)
	}

	// Opposite directions, same step count, same rpm: the wheels counter-rotate and
	// the car turns about its own centre.
	frames := []canbus.Frame{
		protectedMotorFrame(idLMotor, ident.DataIDL, ld, pivotSteps, pivotRPM),
		protectedMotorFrame(idRMotor, ident.DataIDR, rd, pivotSteps, pivotRPM),
	}
	for _, f := range frames {
		if err := ctfSend(f); err != nil {
			logf("error", "pivot routine: drive bus send failed: %v", err)
			return negative(sidRoutineControl, nrcConditionsNotCorrect)
		}
	}
	// Record only AFTER both frames are on the wire, so the snapshot can never
	// advertise a corpus entry that was never transmitted.
	recordSnapshot(frames[0], frames[1])
	logf("cmd", "pivot routine: dir=%#02x steps=%d rpm=%d -> C1 code returned",
		req[4], pivotSteps, pivotRPM)

	// Positive response: 71 <sub> <rid hi> <rid lo> <routineStatusRecord>.
	// The status record is the C1 code. At 4 + 8 = 12 bytes this exceeds a classic
	// 8-byte frame, which is exactly why the DIAG bus is CAN FD -- it rides in one
	// FD single frame and no segmentation layer is needed anywhere.
	resp := []byte{sidRoutineControl + respOffset, sub, byte(rid >> 8), byte(rid)}
	return append(resp, []byte(ident.CodeC1)...)
}
