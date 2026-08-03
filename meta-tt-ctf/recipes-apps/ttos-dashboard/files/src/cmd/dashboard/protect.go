package main

// Message protection for drive commands: CRC-8/SAE-J1850 keyed by a per-motor
// Data ID that is never transmitted.
//
// The scheme is modelled on AUTOSAR E2E Profile 1, which protects a frame with
// exactly this CRC keyed by exactly such a Data ID. §4.1 of the challenge brief
// fixes the CRC parameters but not how the Data ID enters the computation, so
// this file IS the specification -- the motor firmware must match it byte for
// byte, and a mismatch presents as universal silent rejection, which looks
// identical to a bus fault. Change nothing here without reflashing both motors.
//
//	crc8 = CRC8_SAE_J1850( dataid_lo || dataid_hi || <covered bytes> )
//	       poly 0x1D, init 0xFF, xorout 0xFF, no reflection
//
// Data ID prefixed, LOW BYTE FIRST. Prefixing is the AUTOSAR convention and the
// ordering a contestant who knows E2E will try first, which is a feature in a
// car-hacking CTF, not a leak.
//
// WHAT A CONTESTANT CAN ACTUALLY RECOVER: a CRC is linear over GF(2), so a
// fixed-length prefix contributes a constant 8-bit XOR offset independent of the
// payload. Exhaustive search over all 65,536 Data IDs leaves 256 candidates after
// one capture -- and still 256 after three. Extra captures give zero additional
// information. Contestants therefore recover a 256-member equivalence class, every
// member of which produces byte-identical CRCs, so forgery works perfectly without
// ever identifying the true Data ID. Judges must not expect a specific value.

import (
	"encoding/binary"
	"sync/atomic"

	"ttos.local/dashboard/internal/canbus"
)

// crcCoveredLen is how many of the 8 frame bytes the CRC covers.
//
// 7 (bytes 0..6, INCLUDING the nonce) rather than the 6 the brief specifies. This
// is a solvability fix, not a preference. A pivot always sends steps=102, rpm=50
// and dir in {fwd, rev}, so with the CRC covering only bytes 0..5 there are exactly
// TWO distinct CRC inputs capturable per motor, ever, for the whole event -- the
// nonce is the only byte that varies and it would sit outside the covered range.
// Two samples cannot identify a CRC model: sweeping all 256 polynomials x 4
// reflection combinations leaves 4 survivors and no amount of further capture
// resolves it, so the identification step degenerates from analysis into guessing.
// Covering the nonce makes every transmission a fresh (input, output) pair, and
// three captures pin the model uniquely.
//
// Nothing else changes: the nonce stays unvalidated (the motor never inspects its
// value, it only has to be consistent with the CRC), replay still works verbatim,
// and composed C2 frames are captured frames so they remain valid.
const crcCoveredLen = 7

// Pivot geometry and speed, overridable from ttos-dashboard.default.
//
// 102 steps: at 200 full steps/rev the scale is 0.4411 deg/step, so 102 steps is a
// 45 deg pivot. Independent of rpm -- speed does not change the arc.
//
// PIVOT RPM: 75 rpm = 250 steps/sec, chosen 2026-08-03, provisional.
//
// Two constraints pull opposite ways. 50 rpm (167 pps) sits inside the steppers'
// mid-band resonance, so it grinds and DROPS STEPS -- a 102-step pivot then fails
// to turn 45 degrees. 100 rpm (333 pps) is mechanically clean but is exactly the
// speed a contestant would teleoperate at, and since the C2 solve replays captured
// pivot frames carrying this rpm, matching it would let them evade C3 entirely
// without noticing. 75 clears the resonance band and is a speed nobody drives at.
//
// Still needs confirming on a real drivetrain: 250 pps is nearer the band edge than
// the 333 pps measured clean. bench/test-detectors.py holds the executable form of
// the challenge-side constraint.
var (
	pivotSteps uint32 = 102
	pivotRPM   uint32 = 75
)

// crc8J1850 computes CRC-8/SAE-J1850 (poly 0x1D, init 0xFF, xorout 0xFF).
// Check value: crc8J1850([]byte("123456789")) == 0x4B.
func crc8J1850(data []byte) byte {
	crc := byte(0xFF)
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x1D
			} else {
				crc <<= 1
			}
		}
	}
	return crc ^ 0xFF
}

// protectionCRC keys the covered bytes with a Data ID, low byte first.
func protectionCRC(dataID uint16, covered []byte) byte {
	buf := make([]byte, 0, 2+len(covered))
	buf = append(buf, byte(dataID), byte(dataID>>8))
	buf = append(buf, covered...)
	return crc8J1850(buf)
}

// nonceCounter is free-running and shared across both motors. Its VALUE carries no
// meaning -- the motor never inspects it -- but it must change between transmissions
// so that every captured frame is a distinct CRC input. See crcCoveredLen.
var nonceCounter uint32

// protectionValid reports whether an INBOUND drive command carries a correct
// protection field for the motor it is addressed to.
//
// ENFORCEMENT LIVES HERE, ON THE PI, RATHER THAN IN MOTOR FIRMWARE.
//
// The layering doc puts the CRC check in the motor nodes. That cannot coexist with
// two hard requirements: an UNPROVISIONED car must drive, and the car must drive
// after Challenge 3. An unprovisioned Pi has no Data IDs -- they arrive in
// provision.src -- so it cannot construct a protected frame at all, and nodes that
// accept nothing else make the car undrivable until it is provisioned. Putting the
// Data IDs in the image instead would mean fleet secrets in a build artifact.
//
// Moving the check here works because contestants have exactly TWO routes to the
// drive bus and the Pi owns both: the 5-second C2 bridge window and the C3 relay.
// There is no third path -- that is what the gateway policy exists to guarantee.
// So "you must recover the Data ID before you can make the car move" still holds,
// which is the property Challenge 3 is built on.
//
// What it gives up: a node-level check would also stop someone with PHYSICAL
// drive-bus access. That is already outside the threat model -- physical access to
// the drive bus means driving the car directly and skipping every challenge.
//
// What it gains: motor nodes stay BASELINE. Sixteen fewer flashes, no per-node
// challenge build, and the silent-rejection debugging hazard disappears from the
// hardware that is hardest to instrument.
//
// CALLERS MUST DROP SILENTLY. Any observable difference between "rejected" and
// "never arrived" is a CRC oracle: it would let a contestant brute-force the
// protection byte by watching for the frame that stops being refused, instead of
// recovering the Data ID. The only feedback a contestant should get is whether the
// car moved.
func protectionValid(f canbus.Frame) bool {
	if len(f.Data) != 8 {
		return false
	}
	var dataID uint16
	switch f.ID {
	case idLMotor:
		dataID = ident.DataIDL
	case idRMotor:
		dataID = ident.DataIDR
	default:
		return false
	}
	if dataID == 0 {
		// No identity provisioned: there is no challenge on this car, so there is
		// nothing to protect. Refuse anyway -- an inbound path that opens up when
		// provisioning is missing is the wrong failure direction, and a car in that
		// state has no bridge window or relay to reach this code through.
		return false
	}
	return protectionCRC(dataID, f.Data[:crcCoveredLen]) == f.Data[7]
}

// protectedMotorFrame builds the 8-byte protected drive command:
//
//	byte:  0    1    2    3     4     5      6       7
//	     +----+----+----+----+-----+-----+-------+-------+
//	     |   steps (u32 BE)  | dir | rpm | nonce | crc8  |
//	     +----+----+----+----+-----+-----+-------+-------+
//	     |<-------------- covered by crc8 ------>|
//
// Backward compatible with the motor firmware as it stands today: it parses steps
// from bytes 0..3, dir from byte 4 and rpm from byte 5 when DLC >= 6, and ignores
// anything beyond. So a protected frame drives an unflashed node correctly, and
// the CRC only starts being enforced when Phase 3 firmware lands.
func protectedMotorFrame(id uint32, dataID uint16, dir byte, steps, rpm uint32) canbus.Frame {
	if rpm < 1 {
		rpm = 1
	}
	if rpm > 255 {
		rpm = 255
	}
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data[0:4], steps)
	data[4] = dir
	data[5] = byte(rpm)
	data[6] = byte(atomic.AddUint32(&nonceCounter, 1))
	data[7] = protectionCRC(dataID, data[:crcCoveredLen])
	return canbus.Frame{ID: id, Data: data}
}
