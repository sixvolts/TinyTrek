// Command dashboard is the TTOS CTF read-only CAN bus monitor.
//
// It binds a raw SocketCAN socket to each configured interface, and streams every
// frame to connected browsers over Server-Sent Events. The web UI is embedded, so
// the binary is fully self-contained; pass -webdir to serve the UI from disk
// during development (edit index.html, refresh -- no rebuild).
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"ttos.local/dashboard/internal/canbus"
	"ttos.local/dashboard/internal/sse"
)

//go:embed web
var embedded embed.FS

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := flag.String("addr", envOr("TTOS_DASH_ADDR", ":8080"), "listen address")
	ifacesArg := flag.String("ifaces", envOr("TTOS_DASH_IFACES", "can0,can1"), "comma-separated CAN interfaces")
	webdir := flag.String("webdir", os.Getenv("TTOS_DASH_WEBDIR"), "serve UI from this dir instead of the embedded copy (dev)")
	flag.Parse()

	ifaces := splitClean(*ifacesArg)
	if len(ifaces) == 0 {
		log.Fatal("no CAN interfaces configured")
	}

	hub := sse.NewHub()
	go hub.Run()

	for _, ifc := range ifaces {
		go readLoop(ifc, hub)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) { serveEvents(hub, w, r) })
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ifaces": ifaces})
	})
	mux.Handle("/", uiHandler(*webdir))

	log.Printf("ttos-dashboard: ifaces=%v listening on %s", ifaces, *addr)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

// uiHandler serves the web UI from disk (dev) or the embedded FS (prod).
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

// readLoop opens the interface and pumps frames into the hub, retrying the open
// so the dashboard still serves even if the CAN interface isn't up yet at boot.
func readLoop(ifc string, hub *sse.Hub) {
	for {
		conn, err := canbus.Open(ifc)
		if err != nil {
			log.Printf("%s: open failed (%v); retrying in 3s", ifc, err)
			time.Sleep(3 * time.Second)
			continue
		}
		log.Printf("%s: reading", ifc)
		for {
			f, err := conn.ReadFrame()
			if err != nil {
				log.Printf("%s: read error (%v); reopening", ifc, err)
				conn.Close()
				break
			}
			if b, err := json.Marshal(f); err == nil {
				hub.Publish(b)
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func serveEvents(hub *sse.Hub, w http.ResponseWriter, r *http.Request) {
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

	// Open the stream and nudge proxies.
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

func splitClean(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
