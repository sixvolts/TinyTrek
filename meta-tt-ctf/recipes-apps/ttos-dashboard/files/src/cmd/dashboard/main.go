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
	"flag"
	"fmt"
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
	ifacesArg := flag.String("ifaces", envOr("TTOS_DASH_IFACES", "can0,can1"), "comma-separated CAN interfaces")
	webdir := flag.String("webdir", os.Getenv("TTOS_DASH_WEBDIR"), "serve UI from disk instead of the embedded copy (dev)")
	wifiIf := flag.String("wifi", envOr("TTOS_DASH_WIFI_IF", "wlan0"), "interface for the WiFi indicator")
	ethIf := flag.String("eth", envOr("TTOS_DASH_ETH_IF", "eth0"), "interface for the Ethernet indicator")
	demo := flag.Bool("demo", false, "synthesize battery/sensor indicators (arcana mock)")
	driveIf := flag.String("drive", os.Getenv("TTOS_DASH_DRIVE"), "CAN interface to SEND control frames on (empty = read-only)")
	rpmStraight := flag.Uint("rpm", uint(envInt("TTOS_DASH_RPM", 100)), "stepper rpm for straight moves (speed)")
	rpmTurn := flag.Uint("turnrpm", uint(envInt("TTOS_DASH_TURN_RPM", 50)), "stepper rpm for turns/rotate (slower = gentler)")
	keepaliveF := flag.Uint("keepalive", uint(envInt("TTOS_DASH_KEEPALIVE_MS", 150)), "hold-to-drive keepalive interval (ms); each keepalive tops up the node's step buffer")
	stepChunkF := flag.Uint("stepchunk", uint(envInt("TTOS_DASH_STEP_CHUNK", 100)), "steps added to the node buffer per command (~2x per-keepalive consumption; buffer caps at firmware STEPS_MAX ~2.5x this)")
	battID := flag.Uint("battid", uint(envInt("TTOS_DASH_BATT_ID", 0x116)), "CAN ID of BMS battery telemetry")
	battMin := flag.Int("battmin", envInt("TTOS_DASH_BATT_MIN_MV", 8400), "battery millivolts at 0% (resting/open-circuit)")
	battMax := flag.Int("battmax", envInt("TTOS_DASH_BATT_MAX_MV", 12300), "battery millivolts at 100% (resting/open-circuit)")
	battLoad := flag.Int("battload", envInt("TTOS_DASH_BATT_LOAD_OFFSET_MV", 1000), "mV added to the measured (loaded) reading to estimate resting voltage; ~= the steady droop under the Pi load")
	framesArg := flag.String("frames", os.Getenv("TTOS_DASH_FRAMES"), "comma-separated CAN interfaces whose raw frames may be streamed to the UI (empty = none; see frameAllow)")
	flag.Parse()
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
	for _, ifc := range ifaces {
		go readLoop(ifc)
	}
	go statusLoop(*wifiIf, *ethIf, *demo)
	// Pi liveness beacon for the motor nodes' status LEDs. Harmless when control
	// is read-only: it no-ops until a drive socket exists.
	if hb := envInt("TTOS_DASH_HEARTBEAT_MS", 200); hb > 0 {
		go heartbeatLoop(time.Duration(hb) * time.Millisecond)
	}
	if *driveIf != "" {
		go openDrive(*driveIf)
	} else {
		// No explicit drive interface. In factory/test mode driving must work
		// without provisioning; TTOS_DASH_DRIVE is supposed to be set to can0 by
		// first-boot provisioning, but if a boot-ordering hiccup left us reading
		// an empty value we would be stuck read-only until a restart. Self-heal:
		// watch for the factory marker and bring the internal bus up when it
		// appears -- no reboot, no reliance on unit ordering.
		// can1 is the verified drive bus on this hardware (motors + BMS live
		// there); see ttos-provision.sh. Override with TTOS_DASH_FACTORY_DRIVE_IF.
		go factoryDriveWatch(
			envOr("TTOS_DASH_FACTORY_MARKER", "/etc/ttos/factory"),
			envOr("TTOS_DASH_FACTORY_DRIVE_IF", "can1"),
		)
	}

	logf("info", "console starting: ifaces=%s", strings.Join(ifaces, ","))

	mux := http.NewServeMux()
	mux.HandleFunc("/events", serveEvents)
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		// "frames" tells the UI which bus tabs are worth drawing. Without it the UI
		// would render tabs that sit permanently on "Waiting for frames...", which
		// reads as a broken panel rather than a locked one.
		writeJSON(w, map[string]any{
			"ifaces": ifaces, "frames": frameIfaces,
			"car": carInfo(), "repeatMs": repeatMs,
		})
	})
	mux.HandleFunc("/api/control", handleControl)
	mux.Handle("/", uiHandler(*webdir))

	ln := listenRetry(*addr)
	log.Printf("listening on %s", *addr)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.Serve(ln))
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

func publish(v any) {
	if b, err := json.Marshal(v); err == nil {
		hub.Publish(b)
	}
}

// frameAllow lists the interfaces whose raw frames may be streamed to the UI.
// EMPTY BY DEFAULT on a competition car (CTF Phase 0): a locked panel showing live
// DRIVE-bus traffic would hand contestants the C2 corpus by passive sniffing, which
// is exactly what the gateway policy exists to prevent. Set TTOS_DASH_FRAMES on a
// bench image when you want the frame tabs back.
//
// This is a blunt all-or-nothing gate. Phase 5 replaces it with per-session unlock
// tiers, which needs per-client filtering in the SSE hub rather than a global filter
// at the publish site.
var frameAllow = map[string]bool{}

func publishFrame(f canbus.Frame) {
	if !frameAllow[f.Iface] {
		return
	}
	publish(struct {
		Type string       `json:"type"`
		F    canbus.Frame `json:"f"`
	}{"frame", f})
}

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
		bms.Lock()
		bms.on = f.Data[2] == 0x01
		bms.lvc = len(f.Data) >= 4 && f.Data[3]&0x01 != 0
		bms.when, bms.valid = time.Now(), true
		bms.Unlock()
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
	s := statusEvent{Type: "status", USonic: "unknown", Camera: "unknown"}
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !controlCmds[body.Cmd] {
		http.Error(w, "bad command", http.StatusBadRequest)
		return
	}

	var frames []canbus.Frame
	switch body.Cmd {
	case "stop":
		// e-stop: clear both motor buffers (dir 0x00) AND cut the 12V rail.
		setPower(false)
		frames = []canbus.Frame{
			motorFrame(idLMotor, 0x00, 0, straightRPM),
			motorFrame(idRMotor, 0x00, 0, straightRPM),
			bmsFrame(false),
		}
	case "coast":
		// button released: clear the motor buffers so the wheels stop promptly,
		// but leave the 12V rail on so the next press moves without re-arming.
		frames = []canbus.Frame{
			motorFrame(idLMotor, 0x00, 0, straightRPM),
			motorFrame(idRMotor, 0x00, 0, straightRPM),
		}
	default:
		ld, rd := cmdMotors(body.Cmd)
		if !powerOn() {
			frames = append(frames, bmsFrame(true)) // 12V rail on before moving
			setPower(true)
		}
		// Top up each wheel's step buffer (rpm sets the speed).
		steps, rpm := moveParams(body.Cmd)
		frames = append(frames, motorFrame(idLMotor, ld, steps, rpm), motorFrame(idRMotor, rd, steps, rpm))
	}

	if sendDrive(frames) {
		logf("cmd", "control: %s -> sent %s", body.Cmd, describe(frames))
	} else {
		logf("cmd", "control: %s (drive disabled) -> would send %s", body.Cmd, describe(frames))
	}
	writeJSON(w, map[string]any{"ok": true, "cmd": body.Cmd})
}

// ---- drive writer (optional; -drive enables sending on a bus) --------------

var drive struct {
	sync.Mutex
	conn *canbus.Conn
}

func openDrive(iface string) {
	for {
		c, err := canbus.Open(iface)
		if err != nil {
			logf("warn", "drive %s: open failed (%v); retrying in 3s", iface, err)
			time.Sleep(3 * time.Second)
			continue
		}
		drive.Lock()
		drive.conn = c
		drive.Unlock()
		logf("info", "drive bus %s ready -- control is LIVE", iface)
		return
	}
}

// factoryDriveWatch enables the drive bus when the factory/test marker is
// present. It self-heals the first-boot race where the dashboard reads an empty
// TTOS_DASH_DRIVE before provisioning writes it: as soon as the marker exists,
// the internal bus comes up (openDrive retries until the iface is ready) and
// control goes LIVE -- no restart needed. On a provisioned car the marker is
// absent, so this stays a no-op and the console remains read-only by design.
func factoryDriveWatch(marker, iface string) {
	for tries := 0; tries < 60; tries++ { // ~5 min window; factory state is set early in boot
		drive.Lock()
		open := drive.conn != nil
		drive.Unlock()
		if open {
			return // an explicit path already enabled the bus
		}
		if _, err := os.Stat(marker); err == nil {
			logf("info", "factory/test mode (%s present) -- enabling drive on %s", marker, iface)
			openDrive(iface) // blocks retrying until the iface opens, then returns LIVE
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// heartbeatLoop transmits the Pi liveness beacon (0x100) on the drive bus. The
// motor nodes only show READY (green) while this keeps arriving; if the Pi dies
// or the bus is unplugged they fall back to a double-blink red, which is how you
// tell "Pi not talking" apart from "no 12V" at a glance.
//
// It rides the drive socket, so it is only sent when control is LIVE -- a
// read-only car deliberately transmits nothing at all. Send errors are ignored
// here on purpose: at 5 Hz a broken bus would flood the log, and the condition is
// already reported by sendDrive on the next real command (and by the red LEDs).
func heartbeatLoop(period time.Duration) {
	var seq byte
	t := time.NewTicker(period)
	defer t.Stop()
	for range t.C {
		drive.Lock()
		c := drive.conn
		drive.Unlock()
		if c == nil {
			continue
		}
		_ = c.Send(canbus.Frame{ID: idHeartbeat, Data: []byte{seq, 0x00}})
		seq++
	}
}

func sendDrive(frames []canbus.Frame) bool {
	drive.Lock()
	c := drive.conn
	drive.Unlock()
	if c == nil {
		return false
	}
	// Report write failures instead of swallowing them: a down/absent CAN
	// interface would otherwise still be logged as "sent", which makes the console
	// lie about whether a command actually reached the bus.
	for _, f := range frames {
		if err := c.Send(f); err != nil {
			logf("error", "drive TX failed on %X (%v) -- is the CAN interface up?", f.ID, err)
			return false
		}
	}
	return true
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
	if webdir != "" {
		log.Printf("serving UI from disk: %s", webdir)
		return http.FileServer(http.Dir(webdir))
	}
	sub, err := fs.Sub(embedded, "web")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}
	return http.FileServer(http.FS(sub))
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

	client := hub.Add()
	defer hub.Remove(client)

	_, _ = w.Write([]byte(": connected\n\n"))
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
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(msg)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
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
