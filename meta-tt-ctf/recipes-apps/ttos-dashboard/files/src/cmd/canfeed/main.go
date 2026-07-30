// Command canfeed injects synthetic CAN traffic so the dashboard can be developed
// and demoed on arcana without the real vehicle.
//
//	-profile drive : the reused TinyTrek drive protocol on the internal bus --
//	                 0x115 BMS power + 0x111/0x113 L/R motor commands, cycling
//	                 through a plausible forward/turn/reverse/stop sequence.
//	-profile diag  : diagnostic-style traffic on the external bus (extended IDs
//	                 and CAN FD frames).
package main

import (
	"flag"
	"log"
	"math/rand"
	"time"

	"ttos.local/dashboard/internal/canbus"
)

func main() {
	iface := flag.String("iface", "vcan0", "CAN interface to send on")
	profile := flag.String("profile", "drive", "traffic profile: drive|diag")
	rate := flag.Float64("rate", 1.0, "speed multiplier (higher = faster)")
	flag.Parse()

	conn, err := canbus.Open(*iface)
	if err != nil {
		log.Fatalf("open %s: %v (create it: sudo ip link add %s type vcan && sudo ip link set %s up)",
			*iface, err, *iface, *iface)
	}
	defer conn.Close()
	log.Printf("canfeed: profile=%s on %s (rate x%.1f)", *profile, *iface, *rate)

	if *profile == "diag" {
		runDiag(conn, *rate)
	} else {
		runDrive(conn, *rate)
	}
}

// runDrive replays a plausible driving session using the reused protocol.
func runDrive(conn *canbus.Conn, rate float64) {
	type step struct {
		ld, rd byte
		moving bool
		dur    time.Duration
	}
	seq := []step{
		{0x01, 0x01, true, 2500 * time.Millisecond}, // forward
		{0x01, 0x01, false, 800 * time.Millisecond}, // stop
		{0x01, 0x02, true, 1200 * time.Millisecond}, // pivot right (CW)
		{0x01, 0x01, false, 700 * time.Millisecond},
		{0x02, 0x02, true, 1500 * time.Millisecond}, // reverse
		{0x01, 0x01, false, 900 * time.Millisecond},
		{0x02, 0x01, true, 1200 * time.Millisecond}, // pivot left (CCW)
		{0x01, 0x01, false, 1100 * time.Millisecond},
	}
	scale := func(d time.Duration) time.Duration { return time.Duration(float64(d) / rate) }
	refresh := scale(66 * time.Millisecond) // motor command refresh while moving (~15 Hz)

	for {
		for _, s := range seq {
			if s.moving {
				_ = conn.Send(bms(true)) // power on (12V)
				deadline := time.Now().Add(scale(s.dur))
				for time.Now().Before(deadline) {
					_ = conn.Send(motor(0x111, s.ld, 0xFF))
					_ = conn.Send(motor(0x113, s.rd, 0xFF))
					time.Sleep(refresh)
				}
			} else {
				_ = conn.Send(motor(0x111, 0x01, 0x00)) // speed 0 = stop
				_ = conn.Send(motor(0x113, 0x01, 0x00))
				_ = conn.Send(bms(false)) // power off
				time.Sleep(scale(s.dur))
			}
		}
	}
}

func motor(id uint32, dir, speed byte) canbus.Frame {
	return canbus.Frame{ID: id, Data: []byte{0, 0, 0, speed, dir}}
}
func bms(on bool) canbus.Frame {
	v := byte(0x02)
	if on {
		v = 0x01
	}
	return canbus.Frame{ID: 0x115, Data: []byte{v}}
}

// runDiag emits diagnostic-style traffic (extended IDs + CAN FD) on the external bus.
func runDiag(conn *canbus.Conn, rate float64) {
	type gen struct {
		id     uint32
		ext    bool
		fd     bool
		length int
		period time.Duration
	}
	gens := []gen{
		{0x7DF, false, false, 8, 500 * time.Millisecond},     // OBD request (broadcast)
		{0x7E8, false, false, 8, 500 * time.Millisecond},     // OBD response
		{0x18DAF110, true, false, 8, 300 * time.Millisecond}, // UDS (extended)
		{0x400, false, true, 24, 200 * time.Millisecond},     // FD telemetry
	}
	rng := rand.New(rand.NewSource(7))
	for _, g := range gens {
		go func(g gen) {
			p := time.Duration(float64(g.period) / rate)
			if p < time.Millisecond {
				p = time.Millisecond
			}
			t := time.NewTicker(p)
			defer t.Stop()
			ctr := byte(0)
			for range t.C {
				data := make([]byte, g.length)
				rng.Read(data)
				data[0] = ctr
				ctr++
				_ = conn.Send(canbus.Frame{ID: g.id, Ext: g.ext, FD: g.fd, BRS: g.fd, Data: data})
			}
		}(g)
	}
	select {}
}
