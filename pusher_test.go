package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every status the API can answer with, plus the shapes an
// undeployed server produces. The load-bearing property: only a
// delivered achievement is green.
func TestClassifyUnlock(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		reason    string
		wantLevel healthLevel
		wantLabel string // substring; "" means the label must be empty
	}{
		{
			name:      "injected is the only real success",
			status:    "injected",
			wantLevel: healthGreen,
		},
		{
			name:      "already_unlocked is an idempotent no-op, still healthy",
			status:    "already_unlocked",
			wantLevel: healthGreen,
		},
		{
			name:      "no overlay open",
			status:    "no_tracker",
			reason:    "no_active_tracker",
			wantLevel: healthAmber,
			wantLabel: "no StreamTrackr overlay is open",
		},
		{
			name:      "tracker on another platform",
			status:    "no_tracker",
			reason:    "other_platform",
			wantLevel: healthAmber,
			wantLabel: "platform other than Steam",
		},
		{
			name:      "tracker on another game",
			status:    "no_tracker",
			reason:    "other_game",
			wantLevel: healthAmber,
			wantLabel: "different game",
		},
		{
			name:      "no_tracker without a reason still says something useful",
			status:    "no_tracker",
			reason:    "", // production today: the guard branch isn't deployed
			wantLevel: healthAmber,
			wantLabel: "no StreamTrackr tracker can receive",
		},
		{
			name:      "a reason added server-side after this build degrades, not crashes",
			status:    "no_tracker",
			reason:    "some_future_reason",
			wantLevel: healthAmber,
			wantLabel: "no StreamTrackr tracker can receive",
		},
		{
			name:      "tracker still starting",
			status:    "no_baseline",
			wantLevel: healthAmber,
			wantLabel: "still starting up",
		},
		{
			name:      "achievement unknown to the schema",
			status:    "unknown_achievement",
			wantLevel: healthAmber,
			wantLabel: "doesn't know this achievement",
		},
		{
			name:      "a status invented after this build ships is amber, not a crash",
			status:    "some_future_status",
			wantLevel: healthAmber,
			wantLabel: "some_future_status",
		},
		{
			name:      "no status at all is the pre-guard contract: 200 means delivered",
			status:    "",
			wantLevel: healthGreen,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			level, label := classifyUnlock(tc.status, tc.reason)
			if level != tc.wantLevel {
				t.Errorf("level = %s, want %s (label %q)", level, tc.wantLevel, label)
			}
			if tc.wantLabel == "" {
				if label != "" {
					t.Errorf("label = %q, want empty for a healthy delivery", label)
				}
				return
			}
			if !strings.Contains(label, tc.wantLabel) {
				t.Errorf("label = %q, want it to contain %q", label, tc.wantLabel)
			}
		})
	}
}

func TestClassifyHTTPError(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantLabel   string // substring
		wantBlocked bool
	}{
		{
			name:        "403 with an expired-licence code",
			status:      403,
			body:        `{"statusCode":403,"code":"PRO_LICENSE_EXPIRED","message":"Pro licence expired"}`,
			wantLabel:   "Pro subscription has expired",
			wantBlocked: true,
		},
		{
			name:        "403 with a licence-required code",
			status:      403,
			body:        `{"statusCode":403,"code":"PRO_LICENSE_REQUIRED","message":"Pro required"}`,
			wantLabel:   "active StreamTrackr Pro subscription",
			wantBlocked: true,
		},
		{
			name:        "403 with no code — production today",
			status:      403,
			body:        `{"statusCode":403,"message":"Forbidden"}`,
			wantLabel:   "Pro subscription isn't active",
			wantBlocked: true,
		},
		{
			name:        "403 with a body that isn't even JSON",
			status:      403,
			body:        `<html>403 Forbidden</html>`,
			wantLabel:   "Pro subscription isn't active",
			wantBlocked: true,
		},
		{
			name:        "401 points at the fix",
			status:      401,
			body:        `{"statusCode":401,"message":"Unauthorized"}`,
			wantLabel:   "Sign in again",
			wantBlocked: true,
		},
		{
			name:        "429 is transient, not a block",
			status:      429,
			body:        `{}`,
			wantLabel:   "rate-limiting",
			wantBlocked: false,
		},
		{
			name:        "5xx is transient",
			status:      503,
			body:        ``,
			wantLabel:   "having trouble",
			wantBlocked: false,
		},
		{
			name:        "an unexpected 4xx degrades to the raw code",
			status:      418,
			body:        ``,
			wantLabel:   "HTTP 418",
			wantBlocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			label, blocked := classifyHTTPError(tc.status, []byte(tc.body))
			if !strings.Contains(label, tc.wantLabel) {
				t.Errorf("label = %q, want it to contain %q", label, tc.wantLabel)
			}
			if blocked != tc.wantBlocked {
				t.Errorf("blocked = %v, want %v", blocked, tc.wantBlocked)
			}
		})
	}
}

func TestParseUnlockResponse(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
		wantReason string
	}{
		{"full guard response", `{"status":"no_tracker","reason":"other_platform"}`, "no_tracker", "other_platform"},
		{"guard response without reason", `{"status":"no_tracker"}`, "no_tracker", ""},
		{"injected with a tracker slug", `{"status":"injected","trackerSlug":"abc"}`, "injected", ""},
		{"empty body from an older server", ``, "", ""},
		{"garbage body", `not json at all`, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUnlockResponse([]byte(tc.body))
			if got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Errorf("got status=%q reason=%q, want status=%q reason=%q",
					got.Status, got.Reason, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

// Only a tracker caught mid-push is worth asking again: a relock with no
// overlay open is ordinary between speedrun attempts.
func TestRelockRetryable(t *testing.T) {
	for status, want := range map[string]bool{
		"busy":       true,
		"relocked":   false,
		"no_tracker": false,
		"":           false,
	} {
		if got := relockRetryable(status); got != want {
			t.Errorf("relockRetryable(%q) = %v, want %v", status, got, want)
		}
	}
}

// Retrying a 403 forever is what produced thousands of daily requests;
// retrying a network blip is what saves an achievement.
func TestRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503}
	notRetryable := []int{200, 400, 401, 403, 404}
	for _, s := range retryable {
		if !retryableStatus(s) {
			t.Errorf("HTTP %d should be retryable", s)
		}
	}
	for _, s := range notRetryable {
		if retryableStatus(s) {
			t.Errorf("HTTP %d should NOT be retryable", s)
		}
	}
}

// ─── cancellation ──────────────────────────────────────────────────────

// countingServer answers every POST with the given status + body and
// counts the requests it saw.
func countingServer(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// shortRetries collapses the real backoff so a retry test runs in
// milliseconds instead of seconds.
func shortRetries(t *testing.T) {
	t.Helper()
	oldDelay, oldBaseline := pushRetryDelay, baselineRetryDelay
	pushRetryDelay, baselineRetryDelay = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { pushRetryDelay, baselineRetryDelay = oldDelay, oldBaseline })
}

// The achievement was re-locked before the push ever went out. Sending it
// would announce a trophy the player just erased, and leave the server
// unlocked against a locked Steam — with nothing left to detect, since
// both sides then sit still.
func TestPushUnlockCancelledBeforeFirstAttempt(t *testing.T) {
	srv, hits := countingServer(t, 200, `{"status":"injected"}`)

	cancel := make(chan struct{})
	close(cancel)
	pushUnlock(srv.URL, "token", 440, "ACH_A", "", nil, cancel)

	if got := atomic.LoadInt32(hits); got != 0 {
		t.Errorf("sent %d request(s) for an achievement that was already re-locked, want 0", got)
	}
}

// The reset lands while the push is between two attempts: the retry must
// die instead of arriving after the relock.
func TestPushUnlockCancelledDuringBackoff(t *testing.T) {
	shortRetries(t)
	var hits int32
	cancel := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			close(cancel) // the player resets right after the first failure
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	pushUnlock(srv.URL, "token", 440, "ACH_A", "", nil, cancel)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("made %d attempt(s) after the achievement was re-locked, want 1", got)
	}
}

// Nothing was re-locked: a retryable failure still gets its full budget.
func TestPushUnlockWithoutCancelStillRetries(t *testing.T) {
	shortRetries(t)
	srv, hits := countingServer(t, 500, ``)

	pushUnlock(srv.URL, "token", 440, "ACH_A", "", nil, nil)

	if got := atomic.LoadInt32(hits); got != int32(pushAttempts) {
		t.Errorf("made %d attempt(s), want %d", got, pushAttempts)
	}
}

// A reset the streamer asked for, dropped after three tries, used to be a
// log line behind a green dot: `busy` answers 200, which clears the soft
// error, so nothing on screen said the overlay was still on the old run.
func TestPushRelockGivingUpTellsTheUser(t *testing.T) {
	shortRetries(t)
	srv, hits := countingServer(t, 200, `{"status":"busy"}`)
	// `state` is a global: clear it going in, and leave it clean for the
	// next test rather than handing it a stale soft error.
	state.recordRequestOK()
	t.Cleanup(state.recordRequestOK)

	pushRelock(srv.URL, "token", 440, []string{"ACH_A"}, nil)

	if got := atomic.LoadInt32(hits); got != int32(pushAttempts) {
		t.Errorf("made %d attempt(s), want %d", got, pushAttempts)
	}
	if soft := state.snapshot().SoftError; soft == "" {
		t.Error("giving up on a relock left no message for the user")
	}
}

// A server that predates relocks answers 404. That is not the streamer's
// problem and must not paint the tray amber — unlike unlock, whose route
// has shipped for ages.
func TestPushRelockOn404IsSilent(t *testing.T) {
	shortRetries(t)
	srv, hits := countingServer(t, 404, `{"statusCode":404}`)
	state.recordRequestOK()
	t.Cleanup(state.recordRequestOK)

	pushRelock(srv.URL, "token", 440, []string{"ACH_A"}, nil)

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("made %d attempt(s) against a server without the route, want 1", got)
	}
	if soft := state.snapshot().SoftError; soft != "" {
		t.Errorf("404 from an old server surfaced as %q", soft)
	}
}

// postJSON is now the single transport for both pushes: the headers both
// depend on are asserted once, here.
func TestPostJSONSendsAuthAndJSON(t *testing.T) {
	var gotAuth, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"relocked"}`))
	}))
	defer srv.Close()

	status, body, err := postJSON(srv.URL, "tok", "/api/companion/steam/relock",
		relockPayload{AppID: 440, APINames: []string{"ACH_A"}})
	if err != nil || status != 200 {
		t.Fatalf("postJSON: status=%d err=%v", status, err)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("Content-Type = %q", gotType)
	}
	if !strings.Contains(gotBody, `"appId":440`) || !strings.Contains(gotBody, `"ACH_A"`) {
		t.Errorf("body = %q", gotBody)
	}
	if !strings.Contains(string(body), "relocked") {
		t.Errorf("response body = %q", body)
	}
}

// An unreachable host is a network failure, and a network failure is
// worth retrying — the distinction postJSON's stage carries.
func TestPostJSONNetworkErrorIsTagged(t *testing.T) {
	_, _, err := postJSON("http://127.0.0.1:1", "tok", "/api/companion/steam/unlock", unlockPayload{})
	pe, ok := err.(postError)
	if !ok || pe.stage != "network" {
		t.Fatalf("err = %#v, want a postError tagged network", err)
	}
}
