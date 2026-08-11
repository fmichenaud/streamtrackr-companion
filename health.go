package main

import (
	"fmt"
	"runtime"
	"time"
)

// healthLevel is what the tray dot means. It is deliberately about
// delivery, not connectivity:
//
//	green  — the last thing we sent was accepted, or there was nothing
//	         to send and every axis is healthy
//	amber  — we're talking to the API but our pushes go nowhere
//	         (no_tracker / no_baseline / unknown_achievement)
//	red    — the chain is broken and nothing can arrive: no token, a
//	         401/403, no successful request for a while, or we can't
//	         read achievements off the local Steam install at all
//
// Red covers local capture failures too. Strictly, "we can't talk to
// the server" and "we have nothing to send because Steam is unreadable"
// are different layers — but they are the same event for a streamer
// (nothing will ever arrive), and the label says which one it is.
type healthLevel int

const (
	healthGreen healthLevel = iota
	healthAmber
	healthRed
)

func (h healthLevel) String() string {
	switch h {
	case healthGreen:
		return "green"
	case healthAmber:
		return "amber"
	default:
		return "red"
	}
}

// staleAfter is the watchdog threshold: five missed heartbeats at the
// normal 1/min cadence. Below that, a laptop lid or a Wi-Fi hiccup would
// flap the dot for no reason.
const staleAfter = 5 * time.Minute

// staleFor reports how long we've gone without the API accepting a
// request. It measures from the later of "last good response" and "the
// moment this token started being used", so a fresh launch or a fresh
// pairing gets a full grace period instead of an instant red.
func (s stateSnapshot) staleFor(now time.Time) time.Duration {
	ref := s.LastRequestOKAt
	if s.AuthSince.After(ref) {
		ref = s.AuthSince
	}
	if ref.IsZero() {
		return 0
	}
	return now.Sub(ref)
}

// health collapses the three axes into the dot the user sees, plus a
// label written for a streamer rather than for us. Order is by
// severity: a hard block outranks a stale watchdog, which outranks a
// delivery problem.
func (s stateSnapshot) health(now time.Time) (healthLevel, string) {
	switch {
	case !s.Authenticated:
		return healthRed, "not signed in — click “Sign in again”"
	case s.TransportError != "":
		return healthRed, s.TransportError
	case s.CaptureError != "":
		return healthRed, s.CaptureError
	}

	if d := s.staleFor(now); d > staleAfter {
		label := fmt.Sprintf("no reply from StreamTrackr for %s", humanDuration(d))
		if s.SoftError != "" {
			label += " (" + s.SoftError + ")"
		}
		return healthRed, label
	}

	// Delivery problems are sticky on purpose: they stay until the next
	// accepted push. Nothing else tells us the tracker came back, and a
	// problem that clears itself on a timer is a problem that goes
	// unnoticed again.
	if s.DeliveryLevel == healthAmber && s.DeliveryLabel != "" {
		return healthAmber, s.DeliveryLabel
	}

	if s.CurrentAppID == 0 {
		return healthGreen, "waiting for a Steam game"
	}
	return healthGreen, fmt.Sprintf("active · %d unlock(s) this session", s.UnlocksPushedThisSession)
}

// ────────────────────────── health monitor ─────────────────────────

// healthMonitorInterval is how often the watchdog re-evaluates. It runs
// on its own goroutine, reading only the shared state, so it still
// reports the truth when the watcher goroutine it is watching has died,
// hung, or been left holding an empty token.
const healthMonitorInterval = 15 * time.Second

// runHealthMonitor logs every health transition. The tray renders the
// same health() on its own tick; this exists so the transition is
// greppable in a support log ("health: green → red …") even for CLI
// users who have no tray at all.
//
// Only level changes are logged, plus label changes that stay on a
// non-green level (amber "no overlay open" → amber "tracker on another
// game" is a different problem and worth a line). A green label that
// merely counts up — "active · 1 unlock" → "active · 2 unlock" — is not
// a transition, and logging it drowned the real ones.
func runHealthMonitor(done <-chan struct{}) {
	var (
		last      healthLevel
		lastLabel string
		first     = true
	)

	t := time.NewTicker(healthMonitorInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			level, label := state.snapshot().health(time.Now())
			line, worthLogging := healthTransition(first, last, lastLabel, level, label)
			if !worthLogging {
				continue
			}
			logf("%s", line)
			last, lastLabel, first = level, label, false
		}
	}
}

// healthTransition returns the line to log for a health sample, and
// whether it is worth logging at all.
func healthTransition(first bool, last healthLevel, lastLabel string, level healthLevel, label string) (string, bool) {
	switch {
	case first:
		return fmt.Sprintf("health: %s — %s", level, label), true
	case level != last:
		return fmt.Sprintf("health: %s → %s — %s", last, level, label), true
	case level != healthGreen && label != lastLabel:
		return fmt.Sprintf("health: still %s — %s", level, label), true
	default:
		return "", false
	}
}

// ───────────────────────────── supervisor ──────────────────────────

// supervisorRestartDelay throttles the restart loop so a function that
// fails instantly can't spin.
const supervisorRestartDelay = 5 * time.Second

// supervise runs fn on its own goroutine and restarts it if it panics
// or returns while the app is still up. A loop that stops must be
// either restarted or surfaced — the failure being guarded against here
// is a watcher that dies quietly while the tray keeps drawing a healthy
// dot. done closing is the one legitimate way out.
func supervise(name string, done <-chan struct{}, fn func()) {
	go func() {
		for attempt := 1; ; attempt++ {
			outcome := runGuarded(name, fn)

			select {
			case <-done:
				return
			default:
			}

			logf("supervisor: %s %s unexpectedly — restarting in %s (restart #%d)",
				name, outcome, supervisorRestartDelay, attempt)

			select {
			case <-done:
				return
			case <-time.After(supervisorRestartDelay):
			}
		}
	}()
}

// runGuarded turns a panic into a logged outcome instead of a dead
// process. The stack goes to the log: a panic we can't see is a panic
// we can't fix.
func runGuarded(name string, fn func()) (outcome string) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 8192)
			n := runtime.Stack(buf, false)
			logf("panic in %s: %v\n%s", name, r, buf[:n])
			outcome = "panicked"
		}
	}()
	fn()
	return "returned"
}

// ─────────────────────────── formatting ────────────────────────────

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02d", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanRelative(d time.Duration) string {
	return humanDuration(d) + " ago"
}
