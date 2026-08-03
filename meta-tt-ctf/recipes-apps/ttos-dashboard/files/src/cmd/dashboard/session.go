package main

// Phase 5: the panel unlock ladder.
//
// Contestants paste a recovered flag code into the panel; each code raises that
// BROWSER SESSION's tier. Two separate pieces of state, and conflating them is the
// bug this file exists to avoid:
//
//	SESSION tier  -- per browser, in memory, what this client may SEE.
//	CAR record    -- per car, "tiers redeemed since last reset". Drives the BMS
//	                 arm mask and is the judging path.
//
// If the arm mask were driven by session tier, opening a second tab -- or a session
// idling out -- would silently RE-ARM the detectors mid-event and the car would
// start leaking codes again during ordinary driving. If the judging record were
// session-scoped, judges would see their own session rather than the car's history.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tier ladder.
const (
	tierBase = 0 // indicators, bridging tell, docs, flag entry. No raw frames.
	tierC1   = 1 // pivot controls; DIAG-bus frame tab
	tierC2   = 2 // snapshot history, DRIVE-bus frame tab, telematics named outright
	tierC3   = 3 // full drive controls; detectors stood down
)

// Sliding idle, not absolute. A team grinding C2 for two hours must not be
// repeatedly kicked back to locked -- that is punishment for taking the challenge
// seriously, and it generates support load rather than learning.
const sessionIdle = 10 * time.Minute

const sessionCookie = "ttos_session"

type session struct {
	tier     int
	lastSeen time.Time
}

// IN MEMORY, deliberately. A service restart wipes every unlock, which folds
// "clear unlock state" into the existing reset path for free -- there is no
// persistence to forget to clear, and no file for the next team to inherit.
var sessions struct {
	sync.Mutex
	m map[string]*session
}

func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not survivable for an auth token: returning a
		// predictable value would be far worse than refusing to issue one.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// sessionTier returns the caller's current tier, expiring an idle session first
// and sliding the window otherwise.
func sessionTier(r *http.Request) int {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return tierBase
	}
	sessions.Lock()
	defer sessions.Unlock()
	s, ok := sessions.m[c.Value]
	if !ok {
		return tierBase
	}
	if time.Since(s.lastSeen) > sessionIdle {
		delete(sessions.m, c.Value)
		return tierBase
	}
	s.lastSeen = time.Now()
	return s.tier
}

// ensureSession returns the caller's token, minting one if needed.
func ensureSession(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		sessions.Lock()
		_, known := sessions.m[c.Value]
		sessions.Unlock()
		if known {
			return c.Value
		}
	}
	tok := newToken()
	sessions.Lock()
	if sessions.m == nil {
		sessions.m = map[string]*session{}
	}
	sessions.m[tok] = &session{tier: tierBase, lastSeen: time.Now()}
	sessions.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: tok,
		Path:  "/",
		// HttpOnly: the token is never readable from JS, so an XSS in the panel
		// cannot lift another tab's unlock.
		HttpOnly: true,
		// Lax, not None: there is no cross-site flow here, and None would require
		// Secure, which this plain-HTTP origin cannot set.
		SameSite: http.SameSiteLaxMode,
		// NOT Secure: the panel is http:// on the car's AP. Setting Secure would
		// make the browser drop the cookie entirely and nothing would ever unlock.
	})
	return tok
}

func raiseTier(tok string, tier int) {
	sessions.Lock()
	defer sessions.Unlock()
	if s, ok := sessions.m[tok]; ok && tier > s.tier {
		s.tier = tier
		s.lastSeen = time.Now()
	}
}

// resetSessions clears every unlock: session tiers AND the car record. Called by
// the reset path so team two never walks up to a station team one left open.
func resetSessions() {
	sessions.Lock()
	sessions.m = map[string]*session{}
	sessions.Unlock()
	unlocked.Lock()
	unlocked.c2, unlocked.c3 = false, false
	unlocked.Unlock()
	closeBridge()
	logf("info", "RESET: all session unlocks cleared, detectors re-armed")
}

// ---- flag redemption ------------------------------------------------------

// Rate limit per source IP. Flag codes are 8 characters from a restricted
// alphabet, so unthrottled guessing is not a real threat -- but an unthrottled
// endpoint is a free way to hammer the car during an event.
const (
	flagMaxTries   = 10
	flagTryWindow  = 60 * time.Second
	flagMinCodeLen = 8
)

var flagTries struct {
	sync.Mutex
	at map[string][]time.Time
}

func flagThrottled(ip string) bool {
	flagTries.Lock()
	defer flagTries.Unlock()
	if flagTries.at == nil {
		flagTries.at = map[string][]time.Time{}
	}
	cut := time.Now().Add(-flagTryWindow)
	keep := flagTries.at[ip][:0]
	for _, t := range flagTries.at[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	flagTries.at[ip] = append(keep, time.Now())
	return len(flagTries.at[ip]) > flagMaxTries
}

// handleFlag validates a pasted code against THIS CAR ONLY and raises the tier.
func handleFlag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	tok := ensureSession(w, r)
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if flagThrottled(ip) {
		writeJSON(w, map[string]any{"ok": false, "msg": "too many attempts, wait a minute"})
		return
	}

	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, map[string]any{"ok": false, "msg": "bad request"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(body.Code))
	if len(code) != flagMinCodeLen {
		// Malformed vs wrong, same principle as the relay: the length is visible
		// on any code they already hold, so saying it costs nothing and saves a
		// team from debugging a paste error as if it were a wrong answer.
		writeJSON(w, map[string]any{"ok": false, "msg": "codes are 8 characters"})
		return
	}
	if !ident.complete {
		writeJSON(w, map[string]any{"ok": false, "msg": "this car has no challenge identity provisioned"})
		return
	}

	// Constant-time against each of THIS car's codes. Per-car codes are the
	// anti-collusion boundary: a code shouted across the room does nothing here.
	eq := func(a, b string) bool {
		return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
	}
	switch {
	case eq(code, ident.CodeC1):
		raiseTier(tok, tierC1)
		logf("flag", "C1 redeemed from %s -- session tier 1", ip)
		writeJSON(w, map[string]any{"ok": true, "tier": tierC1, "msg": "Challenge 1 unlocked"})
	case eq(code, ident.CodeC2):
		raiseTier(tok, tierC2)
		markUnlocked(2)
		logf("flag", "C2 redeemed from %s -- session tier 2, C2 detector stood down", ip)
		writeJSON(w, map[string]any{"ok": true, "tier": tierC2, "msg": "Challenge 2 unlocked"})
	case eq(code, ident.CodeC3):
		raiseTier(tok, tierC3)
		markUnlocked(3)
		logf("flag", "C3 redeemed from %s -- session tier 3, all detectors stood down", ip)
		writeJSON(w, map[string]any{"ok": true, "tier": tierC3, "msg": "Challenge 3 unlocked -- full drive control"})
	default:
		logf("flag", "rejected code from %s", ip)
		writeJSON(w, map[string]any{"ok": false, "msg": "not a valid code for this car"})
	}
}

// markUnlocked updates the CAR-LEVEL record. This is what the heartbeat arm mask
// reads, so redeeming C2 stands the C2 detector down for the car, not merely for
// the browser tab that did it -- otherwise the next tab re-arms it.
func markUnlocked(tier int) {
	unlocked.Lock()
	if tier >= 2 {
		unlocked.c2 = true
	}
	if tier >= 3 {
		unlocked.c3 = true
	}
	unlocked.Unlock()
}

// carTiers reports the car-level record for judging. Judges cannot read progress
// off the panel -- unlock state is session-scoped, so they would see their own
// session, not the team's.
func carTiers() []int {
	unlocked.Lock()
	defer unlocked.Unlock()
	out := []int{}
	if unlocked.c2 {
		out = append(out, 2)
	}
	if unlocked.c3 {
		out = append(out, 3)
	}
	return out
}

// handleJudge exposes the CAR-LEVEL record. Judges cannot read progress off the
// panel, because unlock state is session-scoped and they would see their own
// session. This is the judging path: what has this car had redeemed since its last
// reset, with the monotonic sequence so verification does not depend on a clock
// that is wrong until an NTP sync which never happens on an AP with no internet.
func handleJudge(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"car":      ident.CarID,
		"vin":      ident.VIN,
		"redeemed": carTiers(),
		"seq":      relaySeqValue(),
	})
}

// handleProvisionNodes pushes the per-node config burst. Operator-initiated, from
// the car itself -- see ttos-provision-nodes. Deliberately NOT gated behind a
// challenge tier: it is a setup action, and the secrets it carries are already in
// this process. It is reachable only on the AP address, like everything else here.
func handleProvisionNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := provisionNodes(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "msg": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "msg": "node config pushed"})
}
