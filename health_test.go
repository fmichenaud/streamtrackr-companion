package main

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// healthy is the baseline "everything works" snapshot; each test bends
// exactly one axis so the assertion names the cause.
func healthy() stateSnapshot {
	return stateSnapshot{
		Authenticated:   true,
		AuthSince:       now.Add(-time.Hour),
		LastRequestOKAt: now.Add(-30 * time.Second),
		DeliveryLevel:   healthGreen,
	}
}

func TestHealthLevels(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*stateSnapshot)
		want      healthLevel
		wantLabel string // substring
	}{
		{
			name:      "no token is red, not green",
			mutate:    func(s *stateSnapshot) { s.Authenticated = false },
			want:      healthRed,
			wantLabel: "not signed in",
		},
		{
			name: "expired Pro licence is red with the server's wording",
			mutate: func(s *stateSnapshot) {
				s.TransportError = "your Pro subscription has expired"
			},
			want:      healthRed,
			wantLabel: "Pro subscription has expired",
		},
		{
			name: "unreadable Steam account is red — nothing can ever be pushed",
			mutate: func(s *stateSnapshot) {
				s.CaptureError = "can't tell which Steam account is signed in"
			},
			want:      healthRed,
			wantLabel: "Steam account",
		},
		{
			name: "no accepted request for longer than the watchdog window is red",
			mutate: func(s *stateSnapshot) {
				s.LastRequestOKAt = now.Add(-staleAfter - time.Minute)
			},
			want:      healthRed,
			wantLabel: "no reply from StreamTrackr",
		},
		{
			name: "a fresh pairing gets a grace period instead of an instant red",
			mutate: func(s *stateSnapshot) {
				s.LastRequestOKAt = time.Time{}
				s.AuthSince = now.Add(-10 * time.Second)
			},
			want:      healthGreen,
			wantLabel: "waiting for a Steam game",
		},
		{
			name: "delivering nowhere is amber, not green",
			mutate: func(s *stateSnapshot) {
				s.DeliveryLevel = healthAmber
				s.DeliveryLabel = "no StreamTrackr overlay is open"
			},
			want:      healthAmber,
			wantLabel: "no StreamTrackr overlay is open",
		},
		{
			name: "a hard block outranks a delivery problem",
			mutate: func(s *stateSnapshot) {
				s.DeliveryLevel = healthAmber
				s.DeliveryLabel = "no StreamTrackr overlay is open"
				s.TransportError = "your session expired"
			},
			want:      healthRed,
			wantLabel: "session expired",
		},
		{
			name:      "idle and healthy",
			mutate:    func(s *stateSnapshot) {},
			want:      healthGreen,
			wantLabel: "waiting for a Steam game",
		},
		{
			name: "in game and healthy reports the session count",
			mutate: func(s *stateSnapshot) {
				s.CurrentAppID = 1091500
				s.UnlocksPushedThisSession = 3
			},
			want:      healthGreen,
			wantLabel: "active · 3 unlock(s)",
		},
		{
			name: "a soft error alone stays green until the watchdog fires",
			mutate: func(s *stateSnapshot) {
				s.SoftError = "network error reaching StreamTrackr"
			},
			want:      healthGreen,
			wantLabel: "waiting",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := healthy()
			tc.mutate(&s)
			got, label := s.health(now)
			if got != tc.want {
				t.Errorf("level = %s, want %s (label %q)", got, tc.want, label)
			}
			if !strings.Contains(label, tc.wantLabel) {
				t.Errorf("label = %q, want it to contain %q", label, tc.wantLabel)
			}
		})
	}
}

// The watchdog must not fire on a state that has never made a request
// and has never been authenticated — that's the CLI diagnostic mode.
func TestStaleForZeroValueIsNotStale(t *testing.T) {
	var s stateSnapshot
	if d := s.staleFor(now); d != 0 {
		t.Errorf("staleFor(zero state) = %s, want 0", d)
	}
}

func TestStaleForUsesTheLaterReference(t *testing.T) {
	s := stateSnapshot{
		LastRequestOKAt: now.Add(-time.Hour),
		AuthSince:       now.Add(-time.Minute), // re-paired one minute ago
	}
	if d := s.staleFor(now); d != time.Minute {
		t.Errorf("staleFor = %s, want 1m (the re-pairing restarts the clock)", d)
	}
}

func TestHeartbeatBackoff(t *testing.T) {
	tests := []struct {
		blocks int
		want   time.Duration
	}{
		{0, time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, heartbeatMaxInterval},
		{50, heartbeatMaxInterval}, // must not overflow the shift
	}
	for _, tc := range tests {
		if got := heartbeatBackoff(tc.blocks); got != tc.want {
			t.Errorf("heartbeatBackoff(%d) = %s, want %s", tc.blocks, got, tc.want)
		}
	}
}

// A permanently-refused companion polled once a minute forever: 1440
// requests a day, none of them useful. Cap the daily volume.
func TestBlockedClientStopsHammering(t *testing.T) {
	const day = 24 * time.Hour
	if perDay := day / heartbeatBackoff(8); perDay > 100 {
		t.Errorf("a blocked client would still send %d requests/day", perDay)
	}
}

// A support log is only useful if a transition stands out. Counting up
// the session's unlocks is not a transition — logging it once buried the
// real ones under "health: green → green" every few seconds.
func TestHealthTransitionOnlyLogsWhatMatters(t *testing.T) {
	tests := []struct {
		name      string
		first     bool
		last      healthLevel
		lastLabel string
		level     healthLevel
		label     string
		wantLog   bool
		wantLine  string
	}{
		{
			name:     "the first sample is always logged",
			first:    true,
			level:    healthGreen,
			label:    "waiting for a Steam game",
			wantLog:  true,
			wantLine: "health: green — waiting for a Steam game",
		},
		{
			name:      "a level change is logged",
			last:      healthGreen,
			lastLabel: "active · 2 unlock(s) this session",
			level:     healthAmber,
			label:     "no StreamTrackr overlay is open",
			wantLog:   true,
			wantLine:  "health: green → amber — no StreamTrackr overlay is open",
		},
		{
			name:      "a different reason for the same amber is logged",
			last:      healthAmber,
			lastLabel: "no StreamTrackr overlay is open",
			level:     healthAmber,
			label:     "your StreamTrackr tracker is following a different game",
			wantLog:   true,
			wantLine:  "health: still amber — your StreamTrackr tracker is following a different game",
		},
		{
			name:      "the unlock counter ticking up is NOT a transition",
			last:      healthGreen,
			lastLabel: "active · 1 unlock(s) this session",
			level:     healthGreen,
			label:     "active · 2 unlock(s) this session",
			wantLog:   false,
		},
		{
			name:      "idle to in-game is NOT a transition either",
			last:      healthGreen,
			lastLabel: "waiting for a Steam game",
			level:     healthGreen,
			label:     "active · 0 unlock(s) this session",
			wantLog:   false,
		},
		{
			name:      "an unchanged sample is silent",
			last:      healthAmber,
			lastLabel: "no StreamTrackr overlay is open",
			level:     healthAmber,
			label:     "no StreamTrackr overlay is open",
			wantLog:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := healthTransition(tc.first, tc.last, tc.lastLabel, tc.level, tc.label)
			if ok != tc.wantLog {
				t.Fatalf("worthLogging = %v, want %v (line %q)", ok, tc.wantLog, line)
			}
			if ok && line != tc.wantLine {
				t.Errorf("line = %q, want %q", line, tc.wantLine)
			}
		})
	}
}

func TestRunGuardedTurnsPanicIntoAnOutcome(t *testing.T) {
	if got := runGuarded("test", func() { panic("boom") }); got != "panicked" {
		t.Errorf("outcome = %q, want %q", got, "panicked")
	}
	if got := runGuarded("test", func() {}); got != "returned" {
		t.Errorf("outcome = %q, want %q", got, "returned")
	}
}
