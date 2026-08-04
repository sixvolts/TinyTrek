package main

// C3: an authenticated raw-CAN relay on the DRIVE bus, reachable over WiFi only.
//
// Functionally socketcand-on-the-drive-bus behind authentication. The pattern is
// reused; the unauthenticated service is NOT -- socketcand was removed from the
// image precisely because it was an unauthenticated network path to the drive bus,
// and re-adding it with a password bolted on would mean one configuration mistake
// away from the original hole.
//
// WHAT THIS DELIBERATELY CANNOT DO: no filesystem access, no command execution, no
// config read. A shell here would hand over the Data ID and collapse the entire
// reverse-engineering content of C2 and C3 in one step. The command surface below
// is the whole protocol -- there is nothing to escape into.

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

// deriveServiceKey is the C3 credential transform. Kept boring on purpose: the
// challenge is assembling the pieces, not the crypto.
//
//	key = SHA-256(vin + ":" + serial + ":" + FLEET_SALT), hex, first 16 chars
//
// 16 hex chars = 64 bits, so brute force is off the table without extra cleverness.
// FLEET_SALT is fleet-wide and ships in the panel JS; the per-car VIN and serial are
// what make each key unique, so a key shouted across the room is useless on another
// car -- that is the anti-collusion boundary.
//
// MUST MATCH deriveServiceKey() in web/index.html EXACTLY. That copy is the one
// contestants actually run, and a mismatch means the intended solution does not
// work while nothing looks broken.
func deriveServiceKey(vin, serial, salt string) string {
	sum := sha256.Sum256([]byte(vin + ":" + serial + ":" + salt))
	return hex.EncodeToString(sum[:])[:16]
}

// ---- rate limiting --------------------------------------------------------

// Per-source-IP, not global: one team fat-fingering their key must not lock out the
// station next to them. Attempts are only counted on FAILURE, so a working client
// reconnecting is never throttled.
const (
	relayMaxFails   = 5
	relayFailWindow = 30 * time.Second
)

var relayFails struct {
	sync.Mutex
	at map[string][]time.Time
}

func relayThrottled(ip string) bool {
	relayFails.Lock()
	defer relayFails.Unlock()
	if relayFails.at == nil {
		relayFails.at = map[string][]time.Time{}
	}
	cut := time.Now().Add(-relayFailWindow)
	keep := relayFails.at[ip][:0]
	for _, t := range relayFails.at[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	relayFails.at[ip] = keep
	return len(keep) >= relayMaxFails
}

func relayNoteFail(ip string) {
	relayFails.Lock()
	defer relayFails.Unlock()
	if relayFails.at == nil {
		relayFails.at = map[string][]time.Time{}
	}
	relayFails.at[ip] = append(relayFails.at[ip], time.Now())
}

// ---- monotonic event sequence ---------------------------------------------

// journald here is volatile and the Pi has no RTC, so timestamps are wrong until an
// NTP sync that never happens on an AP with no internet. Judges verify solves by
// SEQUENCE, not clock. Every consequential relay event carries one.
// relayEndpoint is what DID 0xF1A1 hands back. Set when the relay binds.
var relayEndpoint string

var relaySeq uint64

// relaySeqValue exposes the current sequence for the judging endpoint.
func relaySeqValue() uint64 { return atomic.LoadUint64(&relaySeq) }

func relayLog(format string, a ...any) {
	n := atomic.AddUint64(&relaySeq, 1)
	logf("relay", "[seq=%d car=%s] %s", n, ident.CarID, fmt.Sprintf(format, a...))
}

// ---- server ---------------------------------------------------------------

// relayServe listens for telematics clients. addr is normally the AP address, so
// the relay is reachable over WiFi ONLY -- binding 0.0.0.0 would expose the drive
// bus to anything on the wired network, including the ops LAN at build time.
func relayServe(addr, driveIface string) {
	if !ident.complete {
		logf("warn", "relay not started: CTF identity incomplete (no key to check against)")
		return
	}
	key := deriveServiceKey(ident.VIN, ident.ECUSerial, ident.FleetSalt)

	var ln net.Listener
	for {
		var err error
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			logf("warn", "relay: listen %s failed (%v); retrying in 3s", addr, err)
			time.Sleep(3 * time.Second)
			continue
		}
		break
	}
	relayEndpoint = addr
	logf("info", "relay listening on %s (WiFi only; authenticated raw CAN on %s)", addr, driveIface)

	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go relaySession(c, key, driveIface)
	}
}

// The "read every response" line is not decoration. Every command is acknowledged,
// so a client that pipes input and never reads will fill its receive buffer, and
// closing a socket with unread data sends RST -- which discards whatever the server
// had not yet processed. Measured with busybox nc on the car: 19 of 40 SEND commands
// reached the bus, silently. A client that drains replies gets 40 of 40. Contestants
// will reach for nc first, and "some of my frames vanish" is the worst possible
// thing for them to be debugging during a timed event.
const relayBanner = "TTOS-TELEMATICS 1.0\r\n" +
	"AUTH <key> to begin. Commands: SEND <id>#<hex>, SUB, UNSUB, PING, QUIT\r\n" +
	"NOTE: every command is acknowledged -- READ EACH RESPONSE before sending the\r\n" +
	"next, or your connection may be reset with commands still unprocessed.\r\n" +
	// Sentinel. Without it a client has to KNOW how many banner lines to consume,
	// so adding a line to the banner silently breaks every existing client -- which
	// is exactly what happened here. Read until READY and the banner can grow.
	"READY\r\n"

func relaySession(c net.Conn, key, driveIface string) {
	defer c.Close()
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())

	w := &relayWriter{w: bufio.NewWriter(c)}
	say := func(s string) { _ = w.line(s) }

	if relayThrottled(ip) {
		relayLog("auth throttled for %s", ip)
		say("ERR throttled: too many failed attempts, wait 30s")
		return
	}

	_ = w.writef("%s", relayBanner)

	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	r := bufio.NewReader(c)
	authed := false
	// subStop signals the streaming goroutine to exit; it closes the CAN socket
	// itself. Never close the socket from here -- see relayStream.
	var subStop chan struct{}
	stopSub := func() {
		if subStop != nil {
			close(subStop)
			subStop = nil
		}
	}
	defer stopSub()

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		cmd := strings.ToUpper(fields[0])

		if !authed {
			if cmd != "AUTH" {
				say("ERR not authenticated")
				continue
			}
			if len(fields) != 2 {
				say("ERR malformed: AUTH takes exactly one argument")
				continue
			}
			got := fields[1]
			// MALFORMED vs WRONG, deliberately distinguished.
			//
			// Telling a contestant their key is the wrong LENGTH or not hex costs
			// nothing -- both are visible in the transform they already have -- and
			// it removes the most common source of unfair stalling, where a correct
			// understanding fails on a formatting detail with no feedback. A
			// well-formed but incorrect key gets a generic failure and leaks nothing.
			if len(got) != 16 || !isHex(got) {
				say("ERR malformed: key must be exactly 16 lowercase hex characters")
				continue
			}
			// Constant time: a byte-by-byte compare leaks the key one character at a
			// time to anyone willing to measure, which would turn a 64-bit secret
			// into 16 sequential guesses.
			if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				relayNoteFail(ip)
				relayLog("auth FAILED from %s", ip)
				say("ERR authentication failed")
				if relayThrottled(ip) {
					say("ERR throttled: too many failed attempts, wait 30s")
					return
				}
				continue
			}
			authed = true
			relayLog("auth OK from %s -- drive bus relay open", ip)
			say("OK authenticated -- raw CAN relay on " + driveIface)
			continue
		}

		switch cmd {
		case "PING":
			say("OK pong")

		case "SEND":
			if len(fields) != 2 {
				say("ERR malformed: SEND <id>#<hex>")
				continue
			}
			f, err := parseRelayFrame(fields[1])
			if err != nil {
				say("ERR malformed: " + err.Error())
				continue
			}
			// ctfSend forces FD off. Every node on the drive bus is a classic-only
			// MCP2515 and an FD frame there is a form error that can take the
			// drivetrain bus-off -- a contestant must not be able to brick the car
			// they are attacking, let alone by accident.
			// Protection check, then a UNIFORM answer either way.
			//
			// "OK queued" is deliberately non-committal: it says the relay accepted
			// the command, not that any node acted on it. Answering "bad CRC" here
			// would hand over a free oracle -- a contestant could sweep the
			// protection byte and watch the reply instead of recovering the Data ID,
			// which is the entire challenge. The only signal they get is whether the
			// car moves, exactly as it would be if the motor nodes did the check.
			if protectionValid(f) {
				if err := relaySend(f); err != nil {
					say("ERR send failed: " + err.Error())
					continue
				}
			}
			say("OK queued")

		case "SUB":
			if subStop != nil {
				say("OK already subscribed")
				continue
			}
			conn, err := canbus.Open(driveIface)
			if err != nil {
				say("ERR cannot open " + driveIface)
				continue
			}
			subStop = make(chan struct{})
			relayLog("subscribe from %s", ip)
			go relayStream(conn, c, w, subStop)
			say("OK subscribed")

		case "UNSUB":
			stopSub()
			say("OK unsubscribed")

		case "QUIT":
			say("OK bye")
			return

		default:
			say("ERR unknown command: " + cmd)
		}
	}
}

// relayStream pushes DRIVE-bus frames to a subscribed client until stop is closed.
//
// IT OWNS THE SOCKET AND CLOSES IT ITSELF. UNSUB used to call sub.Close() while
// this goroutine was parked in ReadFrame, on the strength of a comment in canbus
// claiming Close unblocks a pending read. It does not, for a raw fd and a blocking
// read(2): the goroutine and its OS thread stayed parked, one leaked per SUB/UNSUB
// cycle, and the freed fd number was immediately reusable -- so a stranded reader
// could end up draining a socket that canbus.Open had since handed to udsServe.
//
// Instead the socket gets a short read timeout, this loop checks stop between
// polls, and the goroutine that does the reading is the one that does the closing.
func relayStream(conn *canbus.Conn, c net.Conn, w *relayWriter, stop <-chan struct{}) {
	defer conn.Close()
	// Long enough that an idle subscription costs almost nothing, short enough that
	// UNSUB feels instant.
	_ = conn.SetReadTimeout(250 * time.Millisecond)
	for {
		select {
		case <-stop:
			return
		default:
		}
		f, err := conn.ReadFrame()
		if err != nil {
			if canbus.IsTimeout(err) {
				continue // no frame in this window; go back and re-check stop
			}
			return
		}
		// NEVER stream node provisioning. SUB is raw drive-bus read access, and
		// these frames carry the Data IDs -- an authenticated contestant could
		// otherwise lift the answer to Challenge 3 off the wire instead of
		// recovering it, which would make the reverse-engineering content optional.
		if f.ID == idNodeConfig {
			continue
		}
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := w.writef("FRAME %03X#%X\r\n", f.ID, f.Data); err != nil {
			return
		}
	}
}

// relayWriter serialises writes to one client connection.
//
// The command loop and relayStream both write to the same *bufio.Writer, which is
// not safe for concurrent use -- and SUB-then-script-SEND is the normal Challenge 3
// workflow, so the two run together by design. The realistic damage is interleaved
// or dropped output rather than a crash, but a lost "OK queued" is worse than it
// sounds: the banner tells contestants to wait for it, so their client desyncs and
// they read it as their forgery being rejected. That is exactly the unfair stalling
// the relay's retry logic was written to avoid.
type relayWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

func (rw *relayWriter) writef(format string, a ...any) error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if _, err := fmt.Fprintf(rw.w, format, a...); err != nil {
		return err
	}
	return rw.w.Flush()
}

func (rw *relayWriter) line(s string) error { return rw.writef("%s\r\n", s) }

// relaySend transmits with a bounded retry on ENOBUFS.
//
// WHY THIS EXISTS. The drive bus is 500 kbit and the interface transmit queue is
// ten frames deep, so a client that writes commands as fast as TCP will carry them
// overruns the queue in milliseconds -- measured here: 80 frames issued in ~0 ms,
// 29 rejected with ENOBUFS. Without this, a contestant scripting the relay at
// full speed loses drive commands, and the car stutters or does not move. They
// will read that as their forgery being wrong and go back to re-deriving a CRC
// that was correct all along. That is precisely the unfair stalling the challenge
// design sets out to avoid.
//
// Retrying paces the sender to roughly bus speed without blocking forever. The
// bound matters: an unbounded retry would let one client wedge the goroutine if
// the bus really is dead, and "dead bus" is a state this project has been in more
// than once. After the bound we still report the error rather than pretending.
func relaySend(f canbus.Frame) error {
	const (
		retryEvery = 2 * time.Millisecond
		retryFor   = 50 * time.Millisecond
	)
	deadline := time.Now().Add(retryFor)
	for {
		err := ctfSend(f)
		if err == nil || !errors.Is(err, syscall.ENOBUFS) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bus busy: %w (slow down, or the bus is not draining)", err)
		}
		time.Sleep(retryEvery)
	}
}

func parseRelayFrame(s string) (canbus.Frame, error) {
	idStr, dataStr, ok := strings.Cut(s, "#")
	if !ok {
		return canbus.Frame{}, fmt.Errorf("expected <id>#<hex>")
	}
	id, err := strconv.ParseUint(idStr, 16, 32)
	if err != nil {
		return canbus.Frame{}, fmt.Errorf("bad CAN id %q", idStr)
	}
	if id > 0x7FF {
		return canbus.Frame{}, fmt.Errorf("id %03X is not an 11-bit identifier", id)
	}
	// Housekeeping IDs are refused outright. 0x101 carries node provisioning: a
	// contestant who could send it would hand the motors a Data ID they already
	// knew and forge freely, reducing Challenge 3 to "get relay access". 0x100 is
	// the heartbeat, whose flags byte arms and disarms the detectors.
	if uint32(id) == idNodeConfig || uint32(id) == idHeartbeat {
		return canbus.Frame{}, fmt.Errorf("id %03X is reserved", id)
	}
	data, err := hex.DecodeString(dataStr)
	if err != nil {
		return canbus.Frame{}, fmt.Errorf("payload is not hex")
	}
	if len(data) > 8 {
		return canbus.Frame{}, fmt.Errorf("payload is %d bytes; the drive bus is classic CAN (max 8)", len(data))
	}
	return canbus.Frame{ID: uint32(id), Data: data}, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return len(s) > 0
}
