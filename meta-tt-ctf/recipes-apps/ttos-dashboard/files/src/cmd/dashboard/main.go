// Command dashboard is the TinyTrek CTF operator console.
//
// It binds a raw SocketCAN socket to each configured interface and streams three
// kinds of events to the browser over a single Server-Sent Events channel:
//
//	{"type":"frame","f":{...}}   a CAN frame (routed to that bus's tab)
//	{"type":"log",...}           a debug/console message (Debug tab)
//	{"type":"status",...}        indicator state (battery / sensors / network / 12V)
//
// The web UI is embedded, so the binary is self-contained; pass -webdir to serve
// the UI from disk during development.
package main

import (
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ttos.local/dashboard/internal/canbus"
	"ttos.local/dashboard/internal/sse"
)

//go:embed web
var embedded embed.FS

var hub = sse.NewHub()

func main() {
	addr := flag.String("addr", envOr("TTOS_DASH_ADDR", ":8080"), "listen address")
	ifacesArg := flag.String("ifaces", envOr("TTOS_DASH_IFACES", "candrive,candiag"), "comma-separated CAN interfaces")
	webdir := flag.String("webdir", os.Getenv("TTOS_DASH_WEBDIR"), "serve UI from disk instead of the embedded copy (dev)")
	wifiIf := flag.String("wifi", envOr("TTOS_DASH_WIFI_IF", "wlan0"), "interface for the WiFi indicator")
	ethIf := flag.String("eth", envOr("TTOS_DASH_ETH_IF", "eth0"), "interface for the Ethernet indicator")
	demo := flag.Bool("demo", false, "synthesize battery/sensor indicators (arcana mock)")
	// TTOS_DASH_DRIVE is GONE, deliberately. It read as a read-only safety gate and
	// was not one -- see handleControl. Drive authority is the tier-3 session gate.
	// Left unread rather than honoured, so a stale value in an old
	// /etc/default/ttos-dashboard cannot quietly re-disable the control pad.
	rpmStraight := flag.Uint("rpm", uint(envInt("TTOS_DASH_RPM", 100)), "stepper rpm for straight moves (speed)")
	rpmTurn := flag.Uint("turnrpm", uint(envInt("TTOS_DASH_TURN_RPM", 50)), "stepper rpm for turns/rotate (slower = gentler)")
	keepaliveF := flag.Uint("keepalive", uint(envInt("TTOS_DASH_KEEPALIVE_MS", 150)), "hold-to-drive keepalive interval (ms); each keepalive tops up the node's step buffer")
	stepChunkF := flag.Uint("stepchunk", uint(envInt("TTOS_DASH_STEP_CHUNK", 100)), "steps added to the node buffer per command (~2x per-keepalive consumption; buffer caps at firmware STEPS_MAX ~2.5x this)")
	battID := flag.Uint("battid", uint(envInt("TTOS_DASH_BATT_ID", 0x116)), "CAN ID of BMS battery telemetry")
	battMin := flag.Int("battmin", envInt("TTOS_DASH_BATT_MIN_MV", 8400), "battery millivolts at 0% (resting/open-circuit)")
	battMax := flag.Int("battmax", envInt("TTOS_DASH_BATT_MAX_MV", 12300), "battery millivolts at 100% (resting/open-circuit)")
	battLoad := flag.Int("battload", envInt("TTOS_DASH_BATT_LOAD_OFFSET_MV", 1000), "mV added to the measured (loaded) reading to estimate resting voltage; ~= the steady droop under the Pi load")
	framesArg := flag.String("frames", os.Getenv("TTOS_DASH_FRAMES"), "comma-separated CAN interfaces whose raw frames may be streamed to the UI (empty = none; see frameAllow)")
	// CTF service layer. Role names, not numbers: udev names these from the SPI
	// address (10-ttos-can.rules), so DRIVE is always the HAT's CAN0 terminal
	// regardless of kernel probe order. An earlier note here claimed the DRIVE bus
	// was "the higher-numbered interface, verified 2026-08-01"; that was circular
	// -- the Pi transmits on whatever it is configured to use -- and it was wrong.
	ctfEnable := flag.Bool("ctf", envInt("TTOS_CTF_ENABLE", 1) != 0, "run the CTF service layer (heartbeat + diagnostic server)")
	ctfDriveIf := flag.String("ctf-drive", envOr("TTOS_CTF_DRIVE_IF", "candrive"), "DRIVE bus: motors, BMS, heartbeat")
	ctfDiagIf := flag.String("ctf-diag", envOr("TTOS_CTF_DIAG_IF", "candiag"), "DIAG bus: the contestant side tap, UDS server")
	ctfIdentPath := flag.String("ctf-identity", envOr("TTOS_CTF_IDENTITY", "/etc/ttos/provision.src"), "per-car challenge identity (VIN, serial, Data IDs, codes)")
	pivotStepsF := flag.Uint("pivot-steps", uint(envInt("TTOS_PIVOT_STEPS", 102)), "steps per wheel for the C1 pivot routine (102 = 45 deg at 0.4411 deg/step)")
	relayAddrF := flag.String("relay-addr", envOr("TTOS_RELAY_ADDR", ""), "C3 telematics relay listen address (empty = disabled). Bind the AP address only -- 0.0.0.0 exposes the drive bus to the wired network")
	pivotRPMF := flag.Uint("pivot-rpm", uint(envInt("TTOS_PIVOT_RPM", 75)), "rpm for the C1 pivot routine -- see the conflict documented in protect.go before changing")
	flag.Parse()
	pivotSteps = uint32(*pivotStepsF)
	pivotRPM = uint32(*pivotRPMF)
	straightRPM = uint32(*rpmStraight)
	turnRPM = uint32(*rpmTurn)
	repeatMs := uint32(*keepaliveF) // hold-to-drive keepalive; each one refills the node's step buffer
	stepChunk = uint32(*stepChunkF)
	battTelemID = uint32(*battID)
	battMinMv, battMaxMv = *battMin, *battMax
	battLoadMv = *battLoad

	ifaces := splitClean(*ifacesArg)
	if len(ifaces) == 0 {
		log.Fatal("no CAN interfaces configured")
	}

	frameIfaces := splitClean(*framesArg)
	if frameIfaces == nil {
		frameIfaces = []string{} // marshal as [], not null
	}
	for _, s := range frameIfaces {
		frameAllow[s] = true
	}

	go hub.Run()
	go reapSessions()
	for _, ifc := range ifaces {
		go readLoop(ifc)
	}
	go statusLoop(*wifiIf, *ethIf, *demo)

	// ---- CTF service layer -------------------------------------------------
	// Bus ROLE NAMES, not numbers: DRIVE=candrive (motors, BMS), DIAG=candiag (side tap).
	if *ctfEnable {
		loadIdentity(*ctfIdentPath)

		// The one DRIVE-bus socket. Everything that writes to the drive bus goes
		// through it, the control pad included. It must open even while the panel is
		// locked: the heartbeat drives the nodes' status LEDs and the BMS arm mask,
		// and the diagnostic routines have to work on a locked car.
		go openCTFDrive(*ctfDriveIf)

		// Pi liveness beacon (0x100) for the motor nodes' status LEDs. Now
		// unconditional rather than riding the control-pad drive gate.
		if hb := envInt("TTOS_DASH_HEARTBEAT_MS", 200); hb > 0 {
			go heartbeatLoop(time.Duration(hb) * time.Millisecond)
		}

		ctfDiagIface = *ctfDiagIf
		go udsServe(*ctfDiagIf)

		// Re-announce the BMS flag frames onto the DIAG bus in FLAG{...} form.
		// The raw 0x7D1/0x7D2 the gateway forwards are eight bare characters with
		// nothing to mark them as the prize; see flag.go.
		go openFlagAnnounce(*ctfDiagIf)

		// C2 inbound path. Reads the DIAG bus continuously but forwards nothing
		// unless the self-test routine has opened the window; see bridge.go for why
		// this is Go and not a cangw rule.
		go bridgeLoop(*ctfDiagIf)

		// Push per-node config on the FIRST boot after this car was provisioned.
		// Automatic on purpose: nobody should have to shell into eight cars. Not
		// repeated on later boots -- see nodecfg.go for why that matters.
		go autoProvisionNodes()

		// C3 telematics relay. Disabled unless an address is configured, and the
		// address should be the AP address: binding 0.0.0.0 would put an
		// authenticated-but-network-reachable drive bus on the wired side too.
		if *relayAddrF != "" {
			go relayServe(*relayAddrF, *ctfDriveIf)
		} else {
			logf("info", "C3 telematics relay disabled (TTOS_RELAY_ADDR unset)")
		}
	} else {
		logf("warn", "CTF service disabled (TTOS_CTF_ENABLE=0) -- no heartbeat, no diagnostic server")
	}
	// No second drive socket and no factory-mode watcher any more: openCTFDrive
	// above owns the only DRIVE-bus writer, it retries until candrive is up, and it
	// runs in factory and provisioned mode alike. The old pair existed to work
	// around TTOS_DASH_DRIVE arriving empty on first boot -- a race that cannot
	// happen once nothing reads that variable.

	logf("info", "console starting: ifaces=%s", strings.Join(ifaces, ","))

	mux := http.NewServeMux()
	mux.HandleFunc("/events", serveEvents)
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		// Tier-filtered: the UI draws a bus tab only for interfaces this session is
		// allowed to see. A locked panel therefore has NO raw frame tabs -- not
		// hidden by CSS, not empty, absent. The SSE gate is the real enforcement;
		// this keeps the UI honest about what is available.
		// Issue the session here as well as on /events: the panel calls /api/info
		// first, and a client that never gets a cookie can never be raised past
		// tier 0 no matter what code it redeems.
		ensureSession(w, r)
		tier := sessionTier(r)
		visible := []string{}
		for _, i := range frameIfaces {
			if frameTier(i) <= tier {
				visible = append(visible, i)
			}
		}
		// "frames" tells the UI which bus tabs are worth drawing. Without it the UI
		// would render tabs that sit permanently on "Waiting for frames...", which
		// reads as a broken panel rather than a locked one.
		writeJSON(w, map[string]any{
			"ifaces": ifaces, "frames": visible,
			"car": carInfo(), "repeatMs": repeatMs,
			"tier": tier,
		})
	})
	mux.HandleFunc("/api/control", handleControl)
	mux.HandleFunc("/api/flag", handleFlag)
	mux.HandleFunc("/api/provision-nodes", handleProvisionNodes)
	mux.Handle("/", uiHandler(*webdir))

	ln := listenRetry(*addr)
	log.Printf("listening on %s", *addr)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// A SECOND LISTENER ON LOOPBACK, so operator-only endpoints have somewhere to
	// live that contestants on the AP cannot reach. Same mux; handleProvisionNodes
	// is the one handler that additionally requires the request to arrive here.
	// Binding a second address is cheap and needs no second port.
	if _, port, err := net.SplitHostPort(*addr); err == nil {
		go func() {
			lo, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				logf("warn", "loopback listener unavailable (%v) -- ttos-provision-nodes will not work", err)
				return
			}
			logf("info", "operator endpoints on 127.0.0.1:%s", port)
			_ = (&http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}).Serve(lo)
		}()
	}

	log.Fatal(srv.Serve(ln))
}

// isLoopback reports whether a request arrived over the loopback listener.
//
// Used to keep operator-only endpoints off the AP. TCP source addresses are not
// spoofable in any way that survives a handshake, so this is a real boundary
// rather than a hint -- but it is only as good as the bind: never mount an
// endpoint that relies on it behind a wildcard listener.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listenRetry keeps trying to bind (e.g. the AP address may not be up at boot).
func listenRetry(addr string) net.Listener {
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln
		}
		log.Printf("listen %s failed (%v); retrying in 3s", addr, err)
		time.Sleep(3 * time.Second)
	}
}

// ---- event publishers -----------------------------------------------------

// publish sends an event visible at the base (locked) tier: indicators, log lines,
// the bridging tell. Anything privileged must use publishTier.
func publish(v any) { publishTier(tierBase, v) }

func publishTier(tier int, v any) {
	if b, err := json.Marshal(v); err == nil {
		hub.Publish(sse.Msg{Tier: tier, Data: b})
	}
}

// frameAllow lists the interfaces whose raw frames may be streamed to the UI.
// EMPTY BY DEFAULT on a competition car (CTF Phase 0): a locked panel showing live
// DRIVE-bus traffic would hand contestants the C2 corpus by passive sniffing, which
// is exactly what the gateway policy exists to prevent. Set TTOS_DASH_FRAMES on a
// bench image when you want the frame tabs back.
//
// Phase 5 added per-session tier gating, which is now the real control. This stays
// as an explicit KILL SWITCH: with it empty nothing streams regardless of tier, so
// a bug in the tier logic cannot leak the drive bus. Defence in depth, not
// redundancy -- the two gates fail independently.
var frameAllow = map[string]bool{}

// frameTier is the minimum unlock tier for a bus's raw frames.
//
//	DIAG  (candiag)  tier 1 -- contestants already have a physical tap on this bus,
//	                        so showing it after C1 gives away nothing they could
//	                        not sniff themselves.
//	DRIVE (candrive)  tier 2 -- this is the C2 corpus. Showing it earlier would let a
//	                        contestant skip the snapshot DID and the whole
//	                        interact-then-compose shape of C2.
func frameTier(iface string) int {
	if iface == ctfDiagIface {
		return tierC1
	}
	return tierC2
}

func publishFrame(f canbus.Frame) {
	if !frameAllow[f.Iface] {
		return
	}
	// NEVER STREAM THE NODE-CONFIG BURST. 0x101 carries both Data IDs and the C2
	// and C3 codes as plain ASCII (nodecfg.go), and the DRIVE tab unlocks at tier
	// 2 -- so a team that has solved only C2 could read C3 straight off its own
	// panel by triggering a re-provision. The relay's raw-drive reader already
	// drops this ID for exactly the same reason (relay.go); the SSE path, which
	// unlocks a tier EARLIER, did not.
	//
	// Filtered here rather than at the tier gate on purpose: this must hold no
	// matter which tier the DRIVE bus is bound to, and no matter who is watching.
	if f.ID == idNodeConfig {
		return
	}
	publishTier(frameTier(f.Iface), struct {
		Type string       `json:"type"`
		F    canbus.Frame `json:"f"`
	}{"frame", f})
}

// ctfDiagIface is recorded at startup so frameTier can tell the buses apart by
// ROLE rather than by number -- the drive bus is the higher-numbered interface on
// this hardware, and hard-coding a bus number here would invert the gate.
var ctfDiagIface = "candiag"

// logf logs to stderr and to the Debug tab.
func logf(level, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("[%s] %s", level, msg)
	publish(struct {
		Type  string  `json:"type"`
		T     float64 `json:"t"`
		Level string  `json:"level"`
		Msg   string  `json:"msg"`
	}{"log", nowSec(), level, msg})
}

// ---- CAN readers ----------------------------------------------------------

func readLoop(ifc string) {
	for {
		conn, err := canbus.Open(ifc)
		if err != nil {
			logf("warn", "%s: open failed (%v); retrying in 3s", ifc, err)
			time.Sleep(3 * time.Second)
			continue
		}
		logf("info", "%s: up, reading frames", ifc)
		for {
			f, err := conn.ReadFrame()
			if err != nil {
				logf("warn", "%s: read error (%v); reopening", ifc, err)
				conn.Close()
				break
			}
			recordBattery(f)
			// Only the DRIVE-bus original triggers an echo. The gateway also
			// delivers 0x7D1/0x7D2 to DIAG, and announcing on that copy too would
			// double every flag frame on the bus.
			if f.Iface != ctfDiagIface {
				announceFlag(f)
			}
			publishFrame(f)
		}
		time.Sleep(time.Second)
	}
}

// ---- 12V power state -------------------------------------------------------
//
// The BMS (0x115) toggles the 12V rail that powers the motor drivers -- a
// persistent commanded state (on until turned off), and a key element of the
// challenges. We track it directly; the 12V ACTIVE indicator reflects it. A motion
// command powers on if needed; STOP powers off (a true e-stop, since the steppers
// already halt after each move).

var power = struct {
	sync.Mutex
	on bool
}{}

func setPower(on bool) {
	power.Lock()
	power.on = on
	power.Unlock()
}

func powerOn() bool {
	power.Lock()
	defer power.Unlock()
	return power.on
}

// ---- battery telemetry -----------------------------------------------------
//
// The BMS transmits Vbat (millivolts, uint16 big-endian) on battTelemID -- the
// LOADED terminal voltage. We map it to a percentage between battMinMv (0%) and
// battMaxMv (100%), which are RESTING (open-circuit) cell voltages. Because the
// car draws a roughly constant load (the Pi + electronics), the terminal voltage
// sags below resting by ~I*R_internal, a near-constant offset; battLoadMv adds
// that back so a full pack reads ~100% instead of low. Tune the endpoints via
// -battmin/-battmax and the offset via -battload (env TTOS_DASH_BATT_*).

var (
	battTelemID uint32 = 0x116
	battMinMv          = 8400  // 0%   = 2.8 V/cell x3 (resting)
	battMaxMv          = 12300 // 100% = 4.1 V/cell x3 (resting)
	battLoadMv         = 1000  // added to the measured (loaded) reading to estimate resting V
)

var battery = struct {
	sync.Mutex
	mv   int
	when time.Time
}{}

// bms holds the rail state reported BY the BMS itself (beacon byte 2), as opposed
// to the state we commanded. They differ whenever the BMS refuses or drops power
// on its own -- e.g. the low-voltage cutoff -- which is exactly when a truthful
// indicator matters.
var bms = struct {
	sync.Mutex
	on    bool
	lvc   bool
	when  time.Time
	valid bool
}{}

// bmsPower reports the BMS's own rail state and whether that report is fresh.
func bmsPower() (on bool, fresh bool) {
	bms.Lock()
	defer bms.Unlock()
	return bms.on, bms.valid && time.Since(bms.when) < 3*time.Second
}

func recordBattery(f canbus.Frame) {
	if f.ID != battTelemID || len(f.Data) < 2 {
		return
	}
	mv := int(f.Data[0])<<8 | int(f.Data[1])
	battery.Lock()
	battery.mv, battery.when = mv, time.Now()
	battery.Unlock()

	// Beacon form: [v_hi][v_lo][pwr][flags]. Older 2-byte telemetry carries no
	// rail state, so leave the previous reading alone rather than inventing one.
	if len(f.Data) >= 3 {
		railOn := f.Data[2] == 0x01
		bms.Lock()
		bms.on = railOn
		bms.lvc = len(f.Data) >= 4 && f.Data[3]&0x01 != 0
		bms.when, bms.valid = time.Now(), true
		bms.Unlock()

		// RECONCILE THE COMMANDED STATE WITH THE REPORTED ONE. `power.on` is what we
		// last asked for; this is what the BMS says is true. They diverge whenever
		// the BMS drops the rail on its own -- a low-voltage cutoff (which latches
		// and deliberately waits for a fresh 0x115 [0x01]) or any brownout, since
		// its setup() powers off.
		//
		// Without this the cache stays optimistically true, handleControl skips the
		// power-on frame it gates on powerOn(), and every subsequent drive command
		// goes out to unpowered drivers. The only escape was STOP-then-move, which
		// nobody would guess. The ground truth was already decoded here and already
		// preferred by the indicator -- it just never reached the decision.
		if !railOn && powerOn() {
			logf("warn", "BMS reports the 12V rail OFF while we believed it ON "+
				"(low-voltage cutoff or BMS reset) -- the next move will re-energize it")
			setPower(false)
		}
	}
}

// batteryPct returns the charge percent, or nil if there is no recent reading.
func batteryPct() *int {
	battery.Lock()
	mv, when := battery.mv, battery.when
	battery.Unlock()
	if mv == 0 || time.Since(when) > 5*time.Second || battMaxMv <= battMinMv {
		return nil
	}
	// Compensate the loaded reading up to an estimated resting voltage before mapping.
	p := (mv + battLoadMv - battMinMv) * 100 / (battMaxMv - battMinMv)
	if p < 0 {
		p = 0
	} else if p > 100 {
		p = 100
	}
	return &p
}

// ---- indicators -----------------------------------------------------------

type statusEvent struct {
	Type    string `json:"type"`
	Battery *int   `json:"battery"` // percent, null when unknown
	USonic  string `json:"usonic"`  // online|offline|unknown
	Camera  string `json:"camera"`
	WiFi    string `json:"wifi"` // up|down|absent|n/a
	Eth     string `json:"eth"`
	V12     string `json:"v12"` // active|inactive

	// Bridging is C2's FAIRNESS TELL. The panel's indicator flickers to enabled
	// while the self-test holds the inbound path open, so a contestant can see
	// that something happened and correlate it with what they just sent. Without
	// it the window is invisible and the challenge becomes guess-the-timing.
	// It lives in the BASE (locked) tier for that reason -- do not gate it.
	Bridging string `json:"bridging"` // enabled|disabled
}

func statusLoop(wifiIf, ethIf string, demo bool) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		publish(gatherStatus(wifiIf, ethIf, demo))
		<-t.C
	}
}

func gatherStatus(wifiIf, ethIf string, demo bool) statusEvent {
	s := statusEvent{Type: "status", USonic: "unknown", Camera: "unknown",
		Bridging: "disabled"}
	if bridgeOpen() {
		s.Bridging = "enabled"
	}
	s.WiFi = linkState(wifiIf)
	s.Eth = linkState(ethIf)
	// Prefer the BMS's own reported rail state over what we commanded: if the BMS
	// cut power itself (low-voltage cutoff) the indicator must show that, not our
	// optimistic view. Fall back to the commanded state only when no beacon is
	// arriving (older BMS firmware, or the BMS is not on the bus).
	s.V12 = "inactive"
	if on, fresh := bmsPower(); fresh {
		if on {
			s.V12 = "active"
		}
	} else if powerOn() {
		s.V12 = "active"
	}
	s.Battery = batteryPct() // real BMS telemetry (nil if none); demo overrides below
	if demo {
		// A slowly-varying battery and offline sensors, matching the sketch, so the
		// arcana mock shows a fully populated panel. Real sources are wired later.
		b := 78 + int(14*math.Sin(float64(time.Now().Unix())/40.0))
		s.Battery = &b
		s.USonic, s.Camera = "offline", "offline"
		s.WiFi, s.Eth = "up", "down"
	}
	return s
}

// linkState reads the kernel's view of an interface's link.
func linkState(iface string) string {
	if iface == "" {
		return "n/a"
	}
	b, err := os.ReadFile("/sys/class/net/" + iface + "/operstate")
	if err != nil {
		return "absent"
	}
	if strings.TrimSpace(string(b)) == "up" {
		return "up"
	}
	if c, _ := os.ReadFile("/sys/class/net/" + iface + "/carrier"); strings.TrimSpace(string(c)) == "1" {
		return "up"
	}
	return "down"
}

// ---- controls -------------------------------------------------------------

var controlCmds = map[string]bool{
	"forward": true, "back": true, "left": true, "right": true,
	"cw": true, "ccw": true, "stop": true, "coast": true,
}

// TinyTrek reused protocol (from the public repo firmware -- no firmware change):
//
//	0x111 = left motor, 0x113 = right motor: [steps:uint32 big-endian][dir]
//	        the node moves <steps> steps then stops (stepper, blocking).
//	        dir 0x02 = reverse, anything else = forward.
//	0x115 = BMS power: [0x01] = 12V rail on, [0x02] = off
const (
	idHeartbeat = 0x100 // Pi -> nodes: liveness beacon [seq][flags]
	idLMotor    = 0x111
	idRMotor    = 0x113
	idBMS       = 0x115
)

// Motion tuning. The motor nodes run a CONSUMED STEP BUFFER: each command ADDS
// `stepChunk` steps to a buffer (capped at the firmware's STEPS_MAX, ~2.5x a
// chunk) that the node drains as it steps, so dropped/jittered frames don't stall
// it -- it keeps stepping off the reservoir until refilled. Each keepalive adds
// ~2x what's consumed between frames, so the buffer fills to the cap and rides a
// couple of missed frames. rpm sets speed (straights faster than turns).
// Release/stop sends dir 0x00 to clear the buffer promptly.
var (
	straightRPM uint32 = 100
	turnRPM     uint32 = 50
	stepChunk   uint32 = 100 // steps added per command; buffer caps ~2.5x this (firmware STEPS_MAX)
)

// moveParams returns the step chunk to add and the rpm byte for a command.
func moveParams(cmd string) (steps uint32, rpm uint32) {
	switch cmd {
	case "left", "right", "cw", "ccw":
		rpm = turnRPM
	default: // forward, back
		rpm = straightRPM
	}
	if rpm < 1 {
		rpm = 1
	}
	if rpm > 255 {
		rpm = 255 // fits one CAN byte
	}
	return stepChunk, rpm
}

// motorFrame builds [steps:uint32 BE][dir][rpm]. The trailing rpm byte lets each
// wheel run at its own speed; firmware predating it (a 5-byte frame) just falls
// back to its built-in default rpm.
// padFrame builds a control-pad command in whichever form this car can produce.
//
// PROTECTED when an identity is provisioned, because the nodes will then be
// configured and enforcing; UNPROTECTED when it is not, because an unprovisioned
// car has no Data IDs, its nodes are therefore permissive, and it still has to
// drive. Same decision on both sides of the bus, driven by the same fact, so the
// two cannot disagree.
func padFrame(id uint32, dataID uint16, dir byte, steps, rpm uint32) canbus.Frame {
	if dataID == 0 {
		return motorFrame(id, dir, steps, rpm)
	}
	return protectedMotorFrame(id, dataID, dir, steps, rpm)
}

func motorFrame(id uint32, dir byte, steps, rpm uint32) canbus.Frame {
	if rpm < 1 {
		rpm = 1
	}
	if rpm > 255 {
		rpm = 255
	}
	data := make([]byte, 6)
	binary.BigEndian.PutUint32(data[0:4], steps)
	data[4] = dir
	data[5] = byte(rpm)
	return canbus.Frame{ID: id, Data: data}
}

func bmsFrame(on bool) canbus.Frame {
	v := byte(0x02)
	if on {
		v = 0x01
	}
	return canbus.Frame{ID: idBMS, Data: []byte{v}}
}

// cmdMotors maps a UI command to left/right motor directions (differential drive).
func cmdMotors(cmd string) (ldir, rdir byte) {
	switch cmd {
	case "forward":
		return 0x01, 0x01
	case "back":
		return 0x02, 0x02
	case "left", "ccw":
		return 0x02, 0x01
	case "right", "cw":
		return 0x01, 0x02
	default: // stop
		return 0x01, 0x01
	}
}

func handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Cmd string `json:"cmd"`
	}
	// Cap the body. These handlers are reachable unauthenticated from the AP, and
	// an unbounded json.Decoder will read as much as a client sends -- a single
	// large POST is enough to OOM a Pi and take the judging record with it.
	// 4 KB is orders of magnitude more than any legitimate request here.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !controlCmds[body.Cmd] {
		http.Error(w, "bad command", http.StatusBadRequest)
		return
	}

	// Tier gate. "stop" is deliberately ungated: an emergency stop must work from a
	// locked panel, from a judge's phone, from a session that just expired. A safety
	// control behind an unlock is not a safety control.
	if body.Cmd != "stop" && sessionTier(r) < tierC3 {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"ok": false,
			"msg": "drive controls are locked -- redeem the Challenge 3 code, or your session expired; re-enter your flag"})
		return
	}

	var frames []canbus.Frame
	switch body.Cmd {
	case "stop":
		// e-stop: clear both motor buffers (dir 0x00) AND cut the 12V rail.
		// Also slam the inbound bridge shut. An e-stop that left a 5 s window open
		// would let queued contestant frames re-command the wheels immediately after
		// the operator hit stop, which is the one moment that must be absolute.
		closeBridge()
		setPower(false)
		// THE RAIL CUT GOES FIRST. It used to be last, behind both motor frames.
		// Cutting 12V is the only thing that stops a car whose step buffers are
		// already full -- the motor sketches drain what they have with no heartbeat
		// check (see ARCHITECTURE section 8) -- so it is the frame that must not be
		// the one dropped. Under the load that makes a transmit fail at all, which
		// is a contestant saturating the drive bus through the C3 relay, it was
		// precisely the one being dropped.
		frames = []canbus.Frame{
			bmsFrame(false),
			padFrame(idLMotor, ident.DataIDL, 0x00, 0, straightRPM),
			padFrame(idRMotor, ident.DataIDR, 0x00, 0, straightRPM),
		}
	case "coast":
		// button released: clear the motor buffers so the wheels stop promptly,
		// but leave the 12V rail on so the next press moves without re-arming.
		frames = []canbus.Frame{
			padFrame(idLMotor, ident.DataIDL, 0x00, 0, straightRPM),
			padFrame(idRMotor, ident.DataIDR, 0x00, 0, straightRPM),
		}
	default:
		ld, rd := cmdMotors(body.Cmd)
		if !powerOn() {
			frames = append(frames, bmsFrame(true)) // 12V rail on before moving
			setPower(true)
		}
		// Top up each wheel's step buffer (rpm sets the speed).
		steps, rpm := moveParams(body.Cmd)
		frames = append(frames, padFrame(idLMotor, ident.DataIDL, ld, steps, rpm),
			padFrame(idRMotor, ident.DataIDR, rd, steps, rpm))
	}

	// ONE WRITER TO THE DRIVE BUS. This used to go through a second, separate
	// socket gated by TTOS_DASH_DRIVE, which ttos-provision empties on every
	// competition car as a "read-only safety gate". It was not one. The heartbeat,
	// node provisioning, the UDS pivot routine, the C2 bridge and the C3 relay all
	// write to the same bus through ctfSend and were never gated -- so the car was
	// never read-only, and the only thing the gate disabled was the operator's own
	// control pad. Including the e-stop below, whose three frames (zero both step
	// buffers, drop the 12V rail) were silently discarded while this handler still
	// answered {"ok":true}. A safety control behind a config flag that the shipping
	// provisioning path turns off is not a safety control.
	//
	// Tier 3 is the gate now, checked above and re-checked per request. That is
	// what ARCHITECTURE.md section 8 describes, and it is the same move already
	// made for TTOS_DASH_FRAMES when per-session tiers replaced the all-or-nothing
	// switch.
	// The e-stop is BEST EFFORT ACROSS ALL FRAMES. Every other command stops at the
	// first failure, because a half-applied move is worse than none. Stopping is the
	// opposite: each frame independently makes the car safer, so give every one of
	// them a chance even if an earlier one failed.
	err := sendDriveFrames(frames, body.Cmd == "stop")
	if err != nil {
		// Log the failure WITHOUT the frame bytes. describe() renders the full
		// 8-byte protected payload including the CRC, and logf publishes at
		// tierBase -- so an unauthenticated AP client watching the debug stream
		// could pump out fresh protected pairs by spamming the ungated e-stop,
		// escaping both the TTOS_DASH_FRAMES kill switch and the tier-2 DRIVE gate
		// with no CAN adapter at all. The count is the useful part anyway.
		logf("error", "control: %s FAILED after %d frame(s) -- %v",
			body.Cmd, len(frames), err)
		w.WriteHeader(http.StatusServiceUnavailable)
		// Say so. Reporting ok:true for frames that never reached the bus is how a
		// dead e-stop looked healthy from the panel and passed its own test.
		writeJSON(w, map[string]any{"ok": false, "cmd": body.Cmd,
			"msg": "the drive bus did not accept the command: " + err.Error()})
		return
	}
	logf("cmd", "control: %s -> sent %d frame(s)", body.Cmd, len(frames))
	writeJSON(w, map[string]any{"ok": true, "cmd": body.Cmd})
}

// sendDriveFrames puts a control burst on the DRIVE bus.
//
// bestEffort=false stops at the first failure and names it: a "forward" whose left
// wheel was written and right wheel was not is a car that turns, and the operator
// needs to know that happened.
//
// bestEffort=true attempts every frame regardless, and reports the first error
// afterwards. Used by the e-stop, where abandoning the burst on frame 1 meant
// abandoning the 12 V cut -- the only real interlock -- in exactly the situation
// that produces the error. See the retry note in ctfSendRetry.
func sendDriveFrames(frames []canbus.Frame, bestEffort bool) error {
	var firstErr error
	for i, f := range frames {
		if err := ctfSendRetry(f); err != nil {
			if errors.Is(err, errNoBus) {
				err = fmt.Errorf("drive bus is not open (frame %d/%d); is the CTF "+
					"layer enabled and candrive up?", i+1, len(frames))
			} else {
				err = fmt.Errorf("TX failed on %03X (frame %d/%d): %w", f.ID, i+1, len(frames), err)
			}
			if !bestEffort {
				return err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func describe(frames []canbus.Frame) string {
	parts := make([]string, 0, len(frames))
	for _, f := range frames {
		parts = append(parts, fmt.Sprintf("%X#%X", f.ID, f.Data))
	}
	return strings.Join(parts, " ")
}

func carInfo() map[string]string {
	host, _ := os.Hostname()
	id := "—"
	if b, err := os.ReadFile("/etc/ttos/car-id"); err == nil {
		id = strings.TrimSpace(string(b))
	} else if strings.HasPrefix(host, "ttos-car-") {
		id = strings.TrimPrefix(host, "ttos-car-")
	}
	// Image variant, surfaced so the UI can shout when a bench image is running.
	// A debug-tweaks image has an empty root password; one reaching the competition
	// floor is a free win for whoever notices, and the hostname alone is easy to
	// miss on a screen someone is only glancing at.
	variant := "unknown"
	if b, err := os.ReadFile("/etc/ttos-variant"); err == nil {
		variant = strings.TrimSpace(string(b))
	}
	return map[string]string{"host": host, "id": id, "variant": variant}
}

// ---- http plumbing --------------------------------------------------------

func uiHandler(webdir string) http.Handler {
	var inner http.Handler
	if webdir != "" {
		log.Printf("serving UI from disk: %s", webdir)
		inner = http.FileServer(http.Dir(webdir))
	} else {
		sub, err := fs.Sub(embedded, "web")
		if err != nil {
			log.Fatalf("embed: %v", err)
		}
		inner = http.FileServer(http.FS(sub))
	}
	return saltSubstituter(inner)
}

// saltSubstituter rewrites the FLEET_SALT placeholder in index.html at serve time.
//
// The salt is PROVISIONED, not built in, so it can rotate without rebuilding the
// image -- and it is not a secret: the challenge design deliberately ships the key
// derivation to the client, because "auth logic shipped to the browser" is the real
// bug class being taught. Baking it into the embedded asset at build time would
// mean an image rebuild to rotate it, and a per-fleet image.
//
// One bundle is served to every tier. There are no tier-dependent assets, so the
// transform is readable from the locked panel by design; ordering is still enforced
// because the transform is useless without the VIN and serial, and those require
// diagnostic-bus work.
func saltSubstituter(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; p != "/" && p != "/index.html" {
			inner.ServeHTTP(w, r)
			return
		}
		rec := &bodyRecorder{ResponseWriter: w}
		inner.ServeHTTP(rec, r)
		out := strings.ReplaceAll(rec.buf.String(), "__TTOS_FLEET_SALT__", ident.FleetSalt)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(rec.status)
		_, _ = io.WriteString(w, out)
	})
}

type bodyRecorder struct {
	http.ResponseWriter
	buf    strings.Builder
	status int
}

func (b *bodyRecorder) WriteHeader(code int) { b.status = code }
func (b *bodyRecorder) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

func serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	// Establish (or refresh) the session BEFORE streaming: the tier is re-read per
	// message below, so a stream opened at tier 3 stops carrying tier-3 payloads
	// the moment the session lapses, rather than running privileged until the
	// browser happens to reconnect.
	ensureSession(w, r)

	client := hub.Add()
	defer hub.Remove(client)

	// EVERY WRITE GETS A DEADLINE, or a client that connects and then stops reading
	// parks this goroutine permanently. Once the socket send buffer and the peer's
	// receive window fill, w.Write blocks in the kernel; the loop can then never
	// reach r.Context().Done(), so the deferred hub.Remove never runs and the
	// connection costs a goroutine, a socket, a 256-slot channel and a Hub entry
	// that Run iterates on every single publish -- forever. TCP keepalive does not
	// reap it, and `curl --limit-rate 1` in a loop is a comfortable attack.
	//
	// http.Server.WriteTimeout is the usual answer and is wrong here: it is an
	// absolute deadline from the start of the request, so it would cut every healthy
	// SSE stream after N seconds. ResponseController sets it per write instead.
	rc := http.NewResponseController(w)
	deadlined := func(b []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return false
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		return true
	}

	if !deadlined([]byte(": connected\n\n")) {
		return
	}
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-client:
			if !ok {
				return
			}
			// RE-CHECK PER EVENT. Gating only at connect time would leave a
			// long-lived SSE stream carrying privileged frames for as long as the
			// browser held it open -- which is the whole afternoon.
			if msg.Tier > sessionTier(r) {
				continue
			}
			if !deadlined([]byte("data: ")) || !deadlined(msg.Data) || !deadlined([]byte("\n\n")) {
				return
			}
			flusher.Flush()
		case <-ping.C:
			// The ping is also the liveness probe: a client that has stopped reading
			// fails here within the deadline even when no frames are flowing.
			if !deadlined([]byte(": ping\n\n")) {
				return
			}
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func nowSec() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func splitClean(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
