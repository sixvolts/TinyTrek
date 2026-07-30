// Command canfeed injects synthetic CAN traffic onto an interface (default vcan0)
// so the dashboard can be developed and demoed on arcana without the real vehicle.
// It mimics a handful of periodic "ECUs" plus an occasional CAN FD frame.
package main

import (
	"flag"
	"log"
	"math/rand"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

type ecu struct {
	id     uint32
	ext    bool
	fd     bool
	length int
	period time.Duration
	label  string
}

func main() {
	iface := flag.String("iface", "vcan0", "CAN interface to send on")
	rate := flag.Float64("rate", 1.0, "speed multiplier (higher = faster)")
	flag.Parse()

	conn, err := canbus.Open(*iface)
	if err != nil {
		log.Fatalf("open %s: %v (did you create it? sudo ip link add %s type vcan && sudo ip link set %s up)",
			*iface, err, *iface, *iface)
	}
	defer conn.Close()
	log.Printf("canfeed: sending synthetic frames on %s (rate x%.1f)", *iface, *rate)

	// A small cast of periodic senders, loosely vehicle-like.
	ecus := []ecu{
		{id: 0x100, length: 8, period: 20 * time.Millisecond, label: "powertrain/rpm"},
		{id: 0x110, length: 8, period: 50 * time.Millisecond, label: "powertrain/speed"},
		{id: 0x200, length: 6, period: 100 * time.Millisecond, label: "body/lights"},
		{id: 0x201, length: 4, period: 200 * time.Millisecond, label: "body/doors"},
		{id: 0x18DAF110, ext: true, length: 8, period: 500 * time.Millisecond, label: "diag/uds-resp"},
		{id: 0x300, fd: true, length: 16, period: 250 * time.Millisecond, label: "fd/telemetry"},
		{id: 0x7DF, length: 8, period: 1000 * time.Millisecond, label: "diag/obd-req"},
	}

	rng := rand.New(rand.NewSource(1)) // deterministic-ish; this is a dev tool
	stop := make(chan struct{})
	for _, e := range ecus {
		go func(e ecu) {
			counter := byte(0)
			p := time.Duration(float64(e.period) / *rate)
			if p < time.Millisecond {
				p = time.Millisecond
			}
			t := time.NewTicker(p)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					data := make([]byte, e.length)
					rng.Read(data)
					data[0] = counter // a moving byte so the UI visibly changes
					counter++
					f := canbus.Frame{
						Iface: *iface,
						ID:    e.id,
						Ext:   e.ext,
						FD:    e.fd,
						BRS:   e.fd,
						Data:  data,
					}
					if err := conn.Send(f); err != nil {
						log.Printf("send %X (%s): %v", e.id, e.label, err)
					}
				}
			}
		}(e)
	}

	select {} // run until killed
}
