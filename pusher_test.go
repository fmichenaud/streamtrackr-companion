package main

import (
	"strings"
	"testing"
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
