package main

import "testing"

// Regression guard for the outage that made a companion go completely
// silent while the tray stayed green.
//
// The old code had two sources of truth: tokenFlag (used by every
// request path) and state.Authenticated (used by the UI). "Sign out"
// cleared both; "Sign in again" updated only the UI flag. A user who
// signed out and back in — the obvious thing to try when something
// looks stuck — ended up with a valid token on disk, a green dot, and
// an empty in-memory token, so pollCurrentGame and pushUnlock returned
// at their first line. No requests, no log lines, until the app was
// restarted.
//
// applyToken is now the only mutator, which is what keeps these two in
// step. If someone reintroduces a separate setter, this test fails.
func TestApplyTokenKeepsTokenAndUIFlagInSync(t *testing.T) {
	t.Cleanup(func() { applyToken("") })

	applyToken("tok-1")
	if got := currentToken(); got != "tok-1" {
		t.Fatalf("currentToken() = %q, want %q", got, "tok-1")
	}
	if !state.snapshot().Authenticated {
		t.Fatal("Authenticated = false with a token in memory — the UI would lie about being connected")
	}

	applyToken("")
	if got := currentToken(); got != "" {
		t.Fatalf("currentToken() = %q, want empty after sign-out", got)
	}
	if state.snapshot().Authenticated {
		t.Fatal("Authenticated = true with no token — the tray would show a signed-in companion that sends nothing")
	}
}

// The exact sequence from the incident: sign out, then re-pair.
func TestSignOutThenSignInAgainRestoresPushing(t *testing.T) {
	t.Cleanup(func() { applyToken("") })

	applyToken("old-token") // paired
	applyToken("")          // "Sign out of StreamTrackr"
	applyToken("new-token") // "Sign in again…" → this is the line that was missing

	if got := currentToken(); got != "new-token" {
		t.Fatalf("currentToken() = %q after re-pairing, want %q — pushes would be dropped silently", got, "new-token")
	}
	if !state.snapshot().Authenticated {
		t.Fatal("re-pairing left the companion marked as signed out")
	}
}

// Re-authenticating restarts the watchdog's grace period and clears a
// stale hard block: the new token deserves a clean slate, otherwise a
// 401 from the previous token would keep the dot red forever.
func TestReAuthClearsPreviousTransportBlock(t *testing.T) {
	t.Cleanup(func() { applyToken("") })

	applyToken("old-token")
	state.recordTransportBlock("your session expired — click “Sign in again”")
	if state.snapshot().TransportError == "" {
		t.Fatal("precondition: expected a recorded transport block")
	}

	applyToken("")          // sign out
	applyToken("new-token") // re-pair

	s := state.snapshot()
	if s.TransportError != "" {
		t.Errorf("TransportError = %q after re-pairing, want it cleared", s.TransportError)
	}
	if s.TransportBlocks != 0 {
		t.Errorf("TransportBlocks = %d after re-pairing, want 0 (backoff must reset)", s.TransportBlocks)
	}
	if s.AuthSince.IsZero() {
		t.Error("AuthSince not set — the watchdog would fire immediately on a fresh pairing")
	}
}
