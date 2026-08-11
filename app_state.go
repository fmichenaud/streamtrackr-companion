package main

import (
	"sync"
	"time"
)

// stateSnapshot is a mutex-free copy of the shared state — safe to pass
// around, safe to compute health() on, safe to compare in tests.
//
// The state tracks three independent axes, because the companion can
// fail on any one of them while the other two look perfect. That is
// exactly how several unrelated outages produced one indistinguishable
// symptom ("green dot, app running, nothing arrives in StreamTrackr"):
//
//	transport — do our requests reach the API and get accepted?
//	delivery  — does the API have anywhere to put what we send?
//	capture   — can we still read achievements off the local Steam install?
//
// The tray dot derives from all three (see health()). It must never
// again mean "a token exists", which is what the old Authenticated bool
// made it mean.
type stateSnapshot struct {
	Authenticated bool
	Mode          string // "auto" | "manual" | "idle"

	CurrentAppID    uint32
	CurrentGameName string

	BaselineUnlocked         uint32
	BaselineTotal            uint32
	UnlocksPushedThisSession uint32

	LastUnlockTitle string
	LastUnlockAt    time.Time

	// ── transport ───────────────────────────────────────────────────
	// AuthSince starts the watchdog's grace period: a companion that
	// just signed in hasn't had time to make a request yet, and must not
	// go red for that.
	AuthSince time.Time
	// LastRequestOKAt is the watchdog's reference point — set only when
	// the API actually answered and accepted us, never when a request
	// was merely started. The old heartbeat conflated the two, which is
	// why a companion that had stopped calling home still looked fine.
	LastRequestOKAt time.Time
	// TransportError is a hard block (401/403): red immediately, without
	// waiting out the watchdog.
	TransportError  string
	TransportBlocks int // consecutive hard blocks — drives the 403 backoff
	// SoftError is a transient failure (network, 5xx). It doesn't turn
	// the dot red on its own; the watchdog does that if it persists.
	SoftError string

	// ── delivery ────────────────────────────────────────────────────
	DeliveryLevel  healthLevel
	DeliveryLabel  string
	DeliveryStatus string // raw API status, for the tooltip and the log
	DeliveryAt     time.Time

	// ── capture ─────────────────────────────────────────────────────
	// CaptureError means we can't read unlocks locally (no Steam path,
	// no resolvable account, no schema). Nothing will ever be pushed
	// while it's set, so it belongs in the dot rather than in the log.
	CaptureError string

	UserEmail       string
	UserDisplayName string
}

// appState is the source of truth shared between the watcher goroutine
// (writer) and the tray UI (reader). All access is mutex-guarded.
type appState struct {
	mu   sync.RWMutex
	data stateSnapshot
}

var state = &appState{data: stateSnapshot{Mode: "idle"}}

func (s *appState) snapshot() stateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *appState) setIdentity(email, displayName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.UserEmail = email
	s.data.UserDisplayName = displayName
}

// setGame resets per-session counters; pass appid=0 for "no game".
func (s *appState) setGame(appid uint32, name string, unlocked, total uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.CurrentAppID = appid
	s.data.CurrentGameName = name
	s.data.BaselineUnlocked = unlocked
	s.data.BaselineTotal = total
	s.data.UnlocksPushedThisSession = 0
}

func (s *appState) recordUnlock(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastUnlockTitle = title
	s.data.LastUnlockAt = time.Now()
	s.data.UnlocksPushedThisSession++
}

func (s *appState) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Mode = mode
}

// setAuthenticated is deliberately not exported to the rest of the app:
// call applyToken instead, so the in-memory token and the flag the UI
// reads can never disagree again. See applyToken's comment.
func (s *appState) setAuthenticated(authed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if authed && !s.data.Authenticated {
		// Restart the watchdog grace period — a fresh pairing gets the
		// same benefit of the doubt as a fresh launch.
		s.data.AuthSince = time.Now()
		s.data.TransportError = ""
		s.data.TransportBlocks = 0
		s.data.SoftError = ""
	}
	s.data.Authenticated = authed
}

// ─────────────────────────── transport ─────────────────────────────

// recordRequestOK is the only thing that feeds the watchdog. Call it
// when the API answered and accepted us — not when a request was sent.
func (s *appState) recordRequestOK() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastRequestOKAt = time.Now()
	s.data.TransportError = ""
	s.data.TransportBlocks = 0
	s.data.SoftError = ""
}

// recordTransportBlock records a hard rejection (401/403). The counter
// drives the request backoff so a permanent 403 stops meaning thousands
// of pointless requests a day.
func (s *appState) recordTransportBlock(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.TransportError = label
	s.data.SoftError = ""
	if s.data.TransportBlocks < 32 {
		s.data.TransportBlocks++
	}
}

// recordSoftError records a transient failure. It doesn't go red by
// itself — the watchdog decides, once it has lasted long enough.
func (s *appState) recordSoftError(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.SoftError = label
}

// ──────────────────────────── delivery ─────────────────────────────

func (s *appState) recordDelivery(level healthLevel, status, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.DeliveryLevel = level
	s.data.DeliveryStatus = status
	s.data.DeliveryLabel = label
	s.data.DeliveryAt = time.Now()
}

// ──────────────────────────── capture ──────────────────────────────

func (s *appState) setCaptureError(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.CaptureError = label
}

func (s *appState) clearCaptureError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.CaptureError = ""
}
