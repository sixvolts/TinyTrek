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
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
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
	flag.Parse()

	ifaces := splitClean(*ifacesArg)
	if len(ifaces) == 0 {
		log.Fatal("no CAN interfaces configured")
	}

	go hub.Run()
	for _, ifc := range ifaces {
		go readLoop(ifc)
	}
	go statusLoop(*wifiIf, *ethIf, *demo)

	logf("info", "console starting: ifaces=%s", strings.Join(ifaces, ","))

	mux := http.NewServeMux()
	mux.HandleFunc("/events", serveEvents)
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ifaces": ifaces, "car": carInfo()})
	})
	mux.HandleFunc("/api/control", handleControl)
	mux.Handle("/", uiHandler(*webdir))

	log.Printf("listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
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

// ---- 12V / motion state ---------------------------------------------------
//
// The 12V rail is "active" while the car has motion enabled -- a key element of
// the challenges. Here it is driven by the control stub: a motion command arms it
// for a short window, STOP clears it. Real telemetry will drive this later.

var motion = struct {
	sync.Mutex
	until time.Time
}{}

func armMotion() {
	motion.Lock()
	motion.until = time.Now().Add(4 * time.Second)
	motion.Unlock()
}

func clearMotion() {
	motion.Lock()
	motion.until = time.Time{}
	motion.Unlock()
}

func motionActive() bool {
	motion.Lock()
	defer motion.Unlock()
	return time.Now().Before(motion.until)
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
	if motionActive() {
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
	// TODO(control phase): translate the command into CAN frame(s) on the drive
	// bus. For now this is a validated, logged stub so the UI wiring is complete.
	if body.Cmd == "stop" {
		clearMotion()
	} else {
		armMotion()
	}
	logf("cmd", "control: %s", body.Cmd)
	writeJSON(w, map[string]any{"ok": true, "cmd": body.Cmd})
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

func splitClean(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
