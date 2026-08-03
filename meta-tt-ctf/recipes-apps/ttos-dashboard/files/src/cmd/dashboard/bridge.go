package main

// The C2 bridge window: a time-boxed, ID-restricted path from the DIAG bus to the
// DRIVE bus, opened as a side effect of the diagnostic self-test routine.
//
// WHY THIS IS IN USERSPACE AND NOT cangw
//
// The layering doc says to add inbound cangw rules for 0x111/0x113 for the window
// and remove them on expiry. Installing kernel gateway rules needs CAP_NET_ADMIN,
// and ttos-dashboard is the one process on the car that contestants can reach over
// the network. Handing it the capability to reconfigure kernel packet routing --
// permanently, so that it can use it for five seconds per solve -- is a much worse
// trade than forwarding two CAN IDs in Go. The unit keeps CAP_NET_RAW and
// CAP_NET_BIND_SERVICE and nothing else.
//
// The forward is otherwise equivalent: same two IDs, same direction, same window.
// Three differences worth knowing:
//
//   - Frames arrive on DRIVE sourced from the Pi rather than gatewayed. Nothing on
//     the bus can tell the difference; CAN has no source address.
//   - Userspace adds sub-millisecond latency. Irrelevant against a 5 s window.
//   - Closing is immediate and unconditional. A cangw rule that fails to be removed
//     leaves the bridge open for the rest of the event; this cannot outlive its
//     deadline because the forwarder re-checks it on every frame.
//
// The shipped cangw policy is untouched and still handles the permanent
// DRIVE -> DIAG flag path. This only ever moves frames the other way.

import (
	"sync"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

// bridgeWindow is how long the self-test routine holds the inbound path open.
// 5 s is the specified value and it is a deliberate difficulty knob: long enough
// to replay a prepared pair, short enough that it must be scripted rather than
// typed. It is also the same as the UDS S3 timer, so a contestant who lets the
// session lapse loses the window at the same moment.
const bridgeWindow = 5 * time.Second

var bridge struct {
	sync.Mutex
	until time.Time
}

// bridgeIDs is the entire inbound allowlist. Note what is NOT here: 0x115, the BMS
// power command. A contestant who could cut the 12 V rail from the diagnostic tap
// could stop the car mid-challenge, and worse, could do it to a car that is not
// theirs from across the room.
var bridgeIDs = map[uint32]bool{
	idLMotor: true,
	idRMotor: true,
}

// openBridge starts (or extends) the window.
func openBridge() time.Time {
	bridge.Lock()
	defer bridge.Unlock()
	bridge.until = time.Now().Add(bridgeWindow)
	return bridge.until
}

func closeBridge() {
	bridge.Lock()
	bridge.until = time.Time{}
	bridge.Unlock()
}

// bridgeOpen reports whether the window is currently open. Time-based rather than
// flag-based on purpose: there is no timer to fail to fire and no state to leak.
func bridgeOpen() bool {
	bridge.Lock()
	defer bridge.Unlock()
	return time.Now().Before(bridge.until)
}

// bridgeLoop forwards allowlisted frames DIAG -> DRIVE while the window is open.
//
// It reads continuously rather than only during the window, because a socket opened
// on demand would miss frames already queued and would make the window's start edge
// depend on how fast a socket can be created. Reading always and gating on the
// deadline makes the edge exact.
func bridgeLoop(diagIface string) {
	var c *canbus.Conn
	for {
		var err error
		c, err = canbus.Open(diagIface)
		if err != nil {
			logf("warn", "bridge: cannot open DIAG bus %s (%v); retrying in 3s", diagIface, err)
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}
	logf("info", "bridge: watching DIAG bus %s (inbound path opens only during the self-test window)", diagIface)

	for {
		f, err := c.ReadFrame()
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !bridgeIDs[f.ID] || !bridgeOpen() {
			continue
		}
		// PROTECTION IS ENFORCED HERE. An unprotected or bad-CRC frame is dropped
		// and nothing is said about it -- see protectionValid() in protect.go for
		// why the check lives on the Pi, and why silence is mandatory. A contestant
		// replaying captured pivot frames passes trivially, because those frames
		// are valid; a contestant who has not recovered the Data ID cannot make the
		// car move, which is the whole of Challenge 3.
		if !protectionValid(f) {
			continue
		}
		// Forward the payload VERBATIM -- protection bytes included and unaltered.
		// The whole C2 solve is composing two captured, still-valid frames; a
		// bridge that rewrote or re-CRC'd anything would either invalidate the
		// contestant's work or do it for them.
		//
		// FD is forced off by ctfSend. Every node on the drive bus is a classic-only
		// MCP2515, and an FD frame there is a form error that can take the
		// drivetrain bus-off. That is the one thing this path must never pass.
		if f.FD {
			continue
		}
		if err := ctfSend(canbus.Frame{ID: f.ID, Data: f.Data}); err != nil {
			logf("warn", "bridge: forward of %#03x failed: %v", f.ID, err)
		}
	}
}

// ---- actuator snapshot ----------------------------------------------------

// snapshot holds the last protected command sent to each motor, which the snapshot
// DID hands back with protection bytes intact.
//
// This is the SANCTIONED CORPUS SOURCE, and it exists so that the gateway does not
// have to leak drive traffic to the DIAG bus for the challenge to be solvable.
// Forwarding 0x111/0x113 outbound would let a contestant build the same corpus by
// passively sniffing, which collapses C2 into a replay exercise. Making them invoke
// the pivot and read it back is the difference between watching and interacting.
var snapshot struct {
	sync.Mutex
	l, r  canbus.Frame
	valid bool
}

func recordSnapshot(l, r canbus.Frame) {
	snapshot.Lock()
	snapshot.l, snapshot.r, snapshot.valid = l, r, true
	snapshot.Unlock()
}

func readSnapshot() (canbus.Frame, canbus.Frame, bool) {
	snapshot.Lock()
	defer snapshot.Unlock()
	return snapshot.l, snapshot.r, snapshot.valid
}
