package main

// Node provisioning over CAN -- pushed ONCE, by an operator, then stored in flash.
//
// The Pi is the only thing that carries per-car data. During provisioning it pushes
// each node what it needs, the node writes it to flash, and at RUNTIME NOTHING IS
// TRANSMITTED. So there is ONE motor binary and ONE BMS binary for the entire
// fleet, no secrets compiled into firmware, and no per-car build step -- changing a
// Data ID or an unlock code is a reprovision, not a reflash.
//
// This also removes the contradiction that briefly forced the CRC check off the
// nodes: a node cannot validate a Data ID it was never given, and compiling one in
// was the only alternative -- which is what made every car's firmware unique.
//
// WHY NOT PERIODIC. An earlier version broadcast this set at 1 Hz forever and
// filtered it out of the relay's read path. That put the value Challenge 3 exists
// to recover permanently on the bus the contestant is attacking, with one Pi-side
// filter standing between them and it. Provisioning happens before the event, under
// operator control, with nobody listening.
//
// WRITE ONCE. A provisioned node ignores further config frames. Otherwise anyone
// reaching the drive bus could hand the motors a Data ID they already knew and forge
// freely, collapsing Challenge 3 into "get relay access". The relay also refuses to
// transmit this ID at all (see parseRelayFrame).
//
// UNCONFIGURED IS PERMISSIVE. A node with nothing stored accepts the unprotected
// 6-byte command, because an UNPROVISIONED CAR MUST DRIVE. That is fail-open, so the
// Pi's inbound gates keep validating protection independently -- the two cover each
// other and neither is load-bearing alone.

import (
	"errors"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

// idNodeConfig sits next to the heartbeat deliberately: both are Pi-to-node
// housekeeping, and neither is ever forwarded to the diagnostic bus.
const idNodeConfig = 0x101

// Config record types.
const (
	cfgDataIDL = 0x01 // [hi][lo]
	cfgDataIDR = 0x02 // [hi][lo]
	cfgCodeC2  = 0x10 // chunked, 6 ASCII per frame
	cfgCodeC3  = 0x11
	cfgTuning  = 0x20 // detector thresholds, so they change without a reflash
)

// SENT ONLY DURING PROVISIONING, NEVER AT RUNTIME.
//
// An earlier version broadcast this set at 1 Hz forever and relied on filtering it
// out of the relay's read path. That was wrong: it put the value Challenge 3 exists
// to recover continuously on the bus a contestant is attacking, with a single
// Pi-side filter as the only thing between them and it.
//
// Now the operator runs `ttos-provision-nodes` before the event, the nodes write
// what they receive to flash, and at runtime nothing is transmitted at all. A node
// that has been provisioned ignores further config frames, so the value cannot be
// overwritten by anyone who reaches the bus later.
const (
	nodeConfigBurst  = 6 * time.Second       // long enough for a node to boot into it
	nodeConfigRepeat = 200 * time.Millisecond
)

func cfgFrame(typ, idx byte, payload ...byte) canbus.Frame {
	d := make([]byte, 0, 8)
	d = append(d, typ, idx)
	d = append(d, payload...)
	return canbus.Frame{ID: idNodeConfig, Data: d}
}

// codeFrames splits an 8-character code into two 6-byte chunks. Classic CAN caps
// the payload at 8 bytes and two of those are the type and index.
func codeFrames(typ byte, code string) []canbus.Frame {
	b := []byte(code)
	if len(b) > 8 {
		b = b[:8]
	}
	for len(b) < 8 {
		b = append(b, 0)
	}
	return []canbus.Frame{
		cfgFrame(typ, 0, b[0:6]...),
		cfgFrame(typ, 1, b[6:8]...),
	}
}

// nodeConfigSet is everything the nodes need, rebuilt each cycle so a live change
// to tuning takes effect without a restart.
func nodeConfigSet() []canbus.Frame {
	f := []canbus.Frame{
		cfgFrame(cfgDataIDL, 0, byte(ident.DataIDL>>8), byte(ident.DataIDL)),
		cfgFrame(cfgDataIDR, 0, byte(ident.DataIDR>>8), byte(ident.DataIDR)),
	}
	f = append(f, codeFrames(cfgCodeC2, ident.CodeC2)...)
	f = append(f, codeFrames(cfgCodeC3, ident.CodeC3)...)
	// Thresholds travel with the config so they are tunable from provisioning
	// rather than compiled in. PIVOT_RPM in particular has to match the Pi exactly
	// -- a divergence makes the car fire 0x7D2 on its own pivot routine and leak
	// the C3 code before anyone attempts the challenge. Sending it removes the
	// possibility of divergence entirely.
	f = append(f, cfgFrame(cfgTuning, 0,
		byte(pivotRPM),           // rpm that legitimate locked traffic uses
		byte(c2PairWindowMS/10),  // C2: same-dir pair window, centiseconds
		byte(c3Count),            // C3: consecutive qualifying commands
		byte(c3WindowMS/100),     // C3: window, tenths of a second
	))
	return f
}

// Detector thresholds. Bench-tuned against real timing before being pushed to
// firmware; see bench/vehicle-emulator.py and bench/test-detectors.py.
const (
	c2PairWindowMS = 250
	c3Count        = 15
	c3WindowMS     = 3000
)

// provisionNodes pushes the config set for a short burst. Operator-initiated only.
//
// Repeating for a few seconds rather than sending once covers the two real cases:
// a node still booting when the command is run, and a frame lost to a busy bus.
// Nodes latch the first complete set they see and ignore the rest.
func provisionNodes() error {
	if !ident.complete {
		return errNoIdentity
	}
	logf("info", "node provisioning: pushing Data IDs, codes and thresholds for %s", nodeConfigBurst)
	deadline := time.Now().Add(nodeConfigBurst)
	n := 0
	for time.Now().Before(deadline) {
		for _, f := range nodeConfigSet() {
			if err := ctfSend(f); err != nil {
				return err
			}
			n++
		}
		time.Sleep(nodeConfigRepeat)
	}
	logf("info", "node provisioning: sent %d frames; nodes store to flash and will ignore further config", n)
	return nil
}

var errNoIdentity = errors.New("this car has no provisioned identity, so there is nothing to push to the nodes")
