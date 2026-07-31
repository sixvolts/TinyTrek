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
	steps := flag.Uint("steps", uint(envInt("TTOS_DASH_STEPS", 255)), "stepper steps per motion command")
	flag.Parse()
	stepsPerMove = uint32(*steps)

	ifaces := splitClean(*ifacesArg)
	if len(ifaces) == 0 {
		log.Fatal("no CAN interfaces configured")
	}

	go hub.Run()
	for _, ifc := range ifaces {
		go readLoop(ifc)
	}
	go statusLoop(*wifiIf, *ethIf, *demo)
	if *driveIf != "" {
		go openDrive(*driveIf)
	}

	logf("info", "console starting: ifaces=%s", strings.Join(ifaces, ","))

	mux := http.NewServeMux()
	mux.HandleFunc("/events", serveEvents)
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ifaces": ifaces, "car": carInfo()})
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

func publishFrame(f canbus.Frame) {
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
	s.V12 = "inactive"
	if powerOn() {
		s.V12 = "active"
	}
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
	"cw": true, "ccw": true, "stop": true,
}

// TinyTrek reused protocol (from the public repo firmware -- no firmware change):
//
//	0x111 = left motor, 0x113 = right motor: [steps:uint32 big-endian][dir]
//	        the node moves <steps> steps then stops (stepper, blocking).
//	        dir 0x02 = reverse, anything else = forward.
//	0x115 = BMS power: [0x01] = 12V rail on, [0x02] = off
const (
	idLMotor = 0x111
	idRMotor = 0x113
	idBMS    = 0x115
)

// stepsPerMove is how far each motion command advances a wheel (set from -steps).
var stepsPerMove uint32 = 255

func motorFrame(id uint32, dir byte, steps uint32) canbus.Frame {
	data := make([]byte, 5)
	binary.BigEndian.PutUint32(data[0:4], steps)
	data[4] = dir
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
	if body.Cmd == "stop" {
		setPower(false)
		frames = []canbus.Frame{bmsFrame(false)} // cut the 12V rail -- e-stop
	} else {
		ld, rd := cmdMotors(body.Cmd)
		if !powerOn() {
			frames = append(frames, bmsFrame(true)) // 12V rail on before moving
			setPower(true)
		}
		// One discrete stepper move per command on each wheel.
		frames = append(frames, motorFrame(idLMotor, ld, stepsPerMove), motorFrame(idRMotor, rd, stepsPerMove))
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

func sendDrive(frames []canbus.Frame) bool {
	drive.Lock()
	c := drive.conn
	drive.Unlock()
	if c == nil {
		return false
	}
	for _, f := range frames {
		_ = c.Send(f)
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
	return map[string]string{"host": host, "id": id}
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
