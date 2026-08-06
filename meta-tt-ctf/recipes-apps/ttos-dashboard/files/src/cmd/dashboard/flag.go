package main

// Flag presentation.
//
// WHY THIS FILE EXISTS
//
// The BMS emits the C2 and C3 codes as eight bare ASCII bytes on the DRIVE bus
// (0x7D1/0x7D2). Eight is not a style choice and it is not negotiable: the drive
// bus is classic CAN, eight bytes IS the payload, and every node on it is a
// classic-only MCP2515. "FLAG{2FQYWXDM}" is fourteen bytes. Making it fit would
// mean shortening the code, or segmenting across frames, and either one is a
// firmware change plus a flash_nuke erase of every BMS in the fleet -- the codes
// are write-once.
//
// So the wrapper goes everywhere the code is PRESENTED, not where it is emitted:
//
//	C1      in the pivot routine's UDS response, on the FD diagnostic bus, which
//	        has room for it (uds.go)
//	C2/C3   re-announced on the diagnostic bus as FD frames on 0x7D5/0x7D6 as soon
//	        as the raw BMS frame is seen on the drive bus (announceFlag, below)
//	panel   returned by /api/flag on a successful redemption (session.go)
//
// The point is the contestant sniffing raw traffic in SavvyCAN or candump, who is
// most of the intended audience. Our own tool labels the code for them; the bus
// does not. Eight printable characters on an otherwise binary bus is not a marker,
// it is something you have to already know to recognise -- which makes solving the
// challenge and KNOWING you solved it two different skills, and only one of them
// is the one under test.

import (
	"strings"
	"sync"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

const (
	flagPrefix = "FLAG{"
	flagSuffix = "}"

	// Raw BMS flag frames on the DRIVE bus. Classic, 8 bytes, bare ASCII.
	idFlagC2 = 0x7D1
	idFlagC3 = 0x7D2

	// Re-announcement IDs on the DIAG bus.
	//
	// Deliberately NOT 0x7D1/0x7D2. The kernel gateway already forwards those two
	// verbatim from DRIVE to DIAG, and putting a second, differently-shaped
	// payload on an ID that already carries one is a puzzle nobody asked for.
	// These sit immediately above them so the same person scanning the same range
	// finds both, and the pair reads as "here is the code, and here is the code
	// labelled" rather than as a contradiction.
	//
	// Leaving the gateway alone also keeps ttos-reset and ttos-selftest correct
	// as written: both assert exactly two gateway rules, and both are the tools
	// the fleet is accepted with.
	idFlagAnnounceC2 = 0x7D5
	idFlagAnnounceC3 = 0x7D6

	// The BMS repeats at 2 Hz while its detector condition holds. This bounds the
	// echo if that ever changes, so a stuck node cannot turn the diag bus into a
	// flag firehose.
	flagAnnounceEvery = 250 * time.Millisecond
)

// wrapFlag renders a code in submission form. Empty in, empty out: an
// unprovisioned car must not advertise "FLAG{}" as though it held something.
func wrapFlag(code string) string {
	if code == "" {
		return ""
	}
	return flagPrefix + code + flagSuffix
}

// unwrapFlag accepts either form.
//
// A team reads FLAG{2FQYWXDM} off the bus and pastes exactly that into the panel,
// because that is what the car showed them. Rejecting it would tell them their
// solve was wrong at the one moment they were right, which is the worst possible
// time to be unhelpful. Bare codes still work -- the walkthrough, the placards and
// every existing test use them.
func unwrapFlag(s string) string {
	if strings.HasPrefix(s, flagPrefix) && strings.HasSuffix(s, flagSuffix) {
		return s[len(flagPrefix) : len(s)-len(flagSuffix)]
	}
	return s
}

// ---- re-announcement on the diagnostic bus ---------------------------------

var flagAnnounce struct {
	sync.Mutex
	conn *canbus.Conn
	last map[uint32]time.Time
}

// openFlagAnnounce binds a transmit socket on the DIAG bus.
//
// Its own socket rather than sharing the UDS server's: udsServe reopens its
// connection on any read error, and a flag echo silently dying because the
// diagnostic server happened to be reconnecting is the kind of intermittent that
// costs a team a challenge and an operator an afternoon.
func openFlagAnnounce(iface string) {
	for {
		c, err := canbus.Open(iface)
		if err != nil {
			logf("warn", "flag announce %s: open failed (%v); retrying in 3s", iface, err)
			time.Sleep(3 * time.Second)
			continue
		}
		flagAnnounce.Lock()
		flagAnnounce.conn = c
		flagAnnounce.last = map[uint32]time.Time{}
		flagAnnounce.Unlock()
		logf("info", "flag announce ready on %s (0x%03X, 0x%03X)", iface, idFlagAnnounceC2, idFlagAnnounceC3)
		return
	}
}

// announceFlag re-emits a raw BMS flag frame onto the DIAG bus in wrapped form.
//
// It echoes THE BYTES THAT WERE ON THE WIRE, not this car's configured code. A
// board provisioned with the wrong code must show the wrong code: substituting
// ident.CodeC2 here would paper over a mis-provisioned BMS and turn a loud,
// obvious fault into a silent one that only surfaces when a team submits a flag
// the scoreboard rejects.
func announceFlag(f canbus.Frame) {
	var id uint32
	switch f.ID {
	case idFlagC2:
		id = idFlagAnnounceC2
	case idFlagC3:
		id = idFlagAnnounceC3
	default:
		return
	}

	code := strings.TrimRight(string(f.Data), "\x00")
	if len(code) != flagMinCodeLen || !printableASCII(code) {
		// Not a code. Say so once per rate-limit window rather than echoing
		// garbage into the contestant's capture.
		logf("warn", "flag frame 0x%03X carried %d bytes that are not a code; not announcing", f.ID, len(f.Data))
		return
	}

	flagAnnounce.Lock()
	c := flagAnnounce.conn
	if c != nil {
		if last, ok := flagAnnounce.last[id]; ok && time.Since(last) < flagAnnounceEvery {
			flagAnnounce.Unlock()
			return
		}
		flagAnnounce.last[id] = time.Now()
	}
	flagAnnounce.Unlock()
	if c == nil {
		return
	}

	// FD, because the payload is fourteen bytes and this is the bus that can carry
	// it. That is the same reason C1's response is an FD frame, and the same
	// reason a classic-only adapter sees nothing on this bus at all.
	if err := c.Send(canbus.Frame{ID: id, FD: true, Data: []byte(wrapFlag(code))}); err != nil {
		logf("warn", "flag announce 0x%03X failed: %v", id, err)
	}
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}
