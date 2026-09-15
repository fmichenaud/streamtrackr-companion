package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// unlockPayload mirrors SteamUnlockDto in api-nestjs/src/companion/dto.
type unlockPayload struct {
	AppID       uint32          `json:"appId"`
	Achievement achievementInfo `json:"achievement"`
}

type achievementInfo struct {
	APIName     string `json:"apiName"`
	DisplayName string `json:"displayName,omitempty"`
}

// unlockResponse covers every 200 shape of POST /api/companion/steam/unlock.
//
// A 200 does NOT mean the unlock landed — that conflation is what let a
// streamer whose tracker was on Xbox push Steam achievements into a
// black hole for weeks while the companion reported success. Only
// "injected" (and the idempotent "already_unlocked") mean delivered.
//
// Reason is only present on no_tracker, and only on servers that ship
// the replay-guard change. It is absent in production today, so every
// caller must read "" as "the server didn't say" — not as a bug.
type unlockResponse struct {
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	TrackerSlug string `json:"trackerSlug,omitempty"`
}

// apiError is the error body. Code is stable but, like Reason, only on
// servers that have the change deployed.
type apiError struct {
	StatusCode int    `json:"statusCode"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Push retries are bounded and in-memory by design. injectSteamUnlock is
// idempotent (it answers already_unlocked), so a retry can't duplicate;
// but a durable queue would replay yesterday's achievements after a
// restart, which is worse than losing one.
// var, not const, so a test can exercise the retry loop without sleeping
// through the real backoff. Nothing outside tests writes them.
var (
	pushAttempts       = 3
	pushRetryDelay     = 3 * time.Second
	baselineRetryDelay = 8 * time.Second
)

// pushOutcome tells the retry loop whether trying again could plausibly
// change anything.
type pushOutcome struct {
	retryable bool
	delay     time.Duration
}

// postError carries which step failed, so the log line stays as precise
// as it was when each push wrote its own transport code.
type postError struct {
	stage string // "marshal", "request" or "network"
	err   error
}

func (e postError) Error() string { return e.err.Error() }

// asPostError keeps an error that didn't come from postJSON from reading
// as stage "" — which logs as `error= msg=…` and, worse, takes the
// non-retryable branch without anyone deciding that.
func asPostError(err error) postError {
	var pe postError
	if errors.As(err, &pe) {
		return pe
	}
	return postError{stage: "unknown", err: err}
}

// postJSON is the transport every push shares: marshal, authenticated
// POST with a 5 s budget, read a bounded body. It deliberately knows
// nothing about statuses — each endpoint classifies its own answers,
// which is where they legitimately differ (relock forgives a 404 from a
// server that predates it; unlock must not, since /steam/unlock has
// shipped for ages and a 404 there means something is actually wrong
// with the route).
func postJSON(backend, token, path string, payload any) (statusCode int, body []byte, err error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, postError{stage: "marshal", err: err}
	}

	url := strings.TrimRight(backend, "/") + path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, postError{stage: "request", err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, postError{stage: "network", err: err}
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, body, nil
}

// pushUnlock POSTs an unlock event, retrying a bounded number of times
// on failures that a retry can fix (network, 5xx, 429) and on
// no_baseline, which explicitly means "the tracker is still starting,
// try later". Runs on its own goroutine; done aborts the backoff so a
// quit isn't held up by a pending retry.
//
// cancel aborts it for a different reason: this very achievement has been
// re-locked on the player's machine while we were retrying. Delivering it
// now would announce a trophy the player just erased and leave the server
// unlocked against a locked Steam — see {@link inflightUnlocks}. Checked
// before every attempt, not only during the backoff, because a push that
// has not gone out yet is precisely the one worth stopping. A nil channel
// never cancels.
func pushUnlock(backend, token string, appid uint32, apiName, displayName string, done, cancel <-chan struct{}) {
	cancelled := func(attempt int) {
		logf("unlock appid=%d api=%q cancelled=relocked attempt=%d/%d", appid, apiName, attempt, pushAttempts)
	}
	for attempt := 1; ; attempt++ {
		select {
		case <-cancel:
			cancelled(attempt)
			return
		case <-done:
			return
		default:
		}
		outcome := pushUnlockOnce(backend, token, appid, apiName, displayName, attempt)
		if !outcome.retryable || attempt >= pushAttempts {
			if outcome.retryable {
				// No recordSoftError here: whatever made the last attempt
				// retryable already recorded its own, more precise label.
				logf("unlock appid=%d api=%q gave up after %d attempts", appid, apiName, pushAttempts)
			}
			return
		}
		select {
		case <-cancel:
			cancelled(attempt)
			return
		case <-done:
			return
		case <-time.After(outcome.delay):
		}
	}
}

func pushUnlockOnce(backend, token string, appid uint32, apiName, displayName string, attempt int) pushOutcome {
	if token == "" {
		// Two ways to get here: CLI diagnostic mode (no token by design),
		// or the in-memory token having been lost while the UI still
		// thought we were signed in. The second one used to be silent in
		// both the log and the UI — never again.
		logf("unlock appid=%d api=%q skipped=no-token", appid, apiName)
		return pushOutcome{}
	}
	status, respBody, err := postJSON(backend, token, "/api/companion/steam/unlock", unlockPayload{
		AppID: appid,
		Achievement: achievementInfo{
			APIName:     apiName,
			DisplayName: displayName,
		},
	})
	if err != nil {
		pe := asPostError(err)
		if pe.stage != "network" {
			logf("unlock appid=%d api=%q error=%s msg=%q", appid, apiName, pe.stage, err.Error())
			return pushOutcome{}
		}
		logf("unlock appid=%d api=%q attempt=%d/%d error=network msg=%q",
			appid, apiName, attempt, pushAttempts, err.Error())
		state.recordSoftError("network error reaching StreamTrackr")
		return pushOutcome{retryable: true, delay: pushRetryDelay}
	}

	if status != http.StatusOK {
		label, blocked := classifyHTTPError(status, respBody)
		logf("unlock appid=%d api=%q attempt=%d/%d http=%d code=%s msg=%q",
			appid, apiName, attempt, pushAttempts, status,
			orDash(parseAPIError(respBody).Code), label)
		if blocked {
			state.recordTransportBlock(label)
			return pushOutcome{}
		}
		state.recordSoftError(label)
		return pushOutcome{retryable: retryableStatus(status), delay: pushRetryDelay}
	}

	// The API answered and accepted us: the watchdog is satisfied even
	// if the unlock itself had nowhere to go.
	state.recordRequestOK()

	decoded := parseUnlockResponse(respBody)
	level, label := classifyUnlock(decoded.Status, decoded.Reason)
	logf("unlock appid=%d api=%q attempt=%d/%d http=200 status=%s reason=%s tracker=%s → %s",
		appid, apiName, attempt, pushAttempts,
		orDash(decoded.Status), orDash(decoded.Reason), orDash(decoded.TrackerSlug), level)
	state.recordDelivery(level, decoded.Status, label)

	if decoded.Status == "no_baseline" {
		return pushOutcome{retryable: true, delay: baselineRetryDelay}
	}
	return pushOutcome{}
}

// relockPayload mirrors SteamRelockDto in api-nestjs/src/companion/dto.
type relockPayload struct {
	AppID    uint32   `json:"appId"`
	APINames []string `json:"apiNames"`
}

// pushRelock POSTs achievements that went from unlocked to locked, with
// the same bounded retries as pushUnlock. Idempotent server-side: a
// relock of something already locked is a no-op.
func pushRelock(backend, token string, appid uint32, apiNames []string, done <-chan struct{}) {
	for attempt := 1; ; attempt++ {
		outcome := pushRelockOnce(backend, token, appid, apiNames, attempt)
		if !outcome.retryable || attempt >= pushAttempts {
			if outcome.retryable {
				logf("relock appid=%d count=%d gave up after %d attempts", appid, len(apiNames), pushAttempts)
				// A reset is something the streamer did on purpose, between
				// two attempts, and is watching for. Giving up on it used to
				// be a log line and a green dot: the overlay kept the old run
				// and nothing said why.
				state.recordSoftError("StreamTrackr couldn't apply your achievement reset — it may still show the old run")
			}
			return
		}
		select {
		case <-done:
			return
		case <-time.After(outcome.delay):
		}
	}
}

func pushRelockOnce(backend, token string, appid uint32, apiNames []string, attempt int) pushOutcome {
	if token == "" {
		logf("relock appid=%d count=%d skipped=no-token", appid, len(apiNames))
		return pushOutcome{}
	}
	status, respBody, err := postJSON(backend, token, "/api/companion/steam/relock",
		relockPayload{AppID: appid, APINames: apiNames})
	if err != nil {
		pe := asPostError(err)
		if pe.stage != "network" {
			logf("relock appid=%d error=%s msg=%q", appid, pe.stage, err.Error())
			return pushOutcome{}
		}
		logf("relock appid=%d attempt=%d/%d error=network msg=%q", appid, attempt, pushAttempts, err.Error())
		state.recordSoftError("network error reaching StreamTrackr")
		return pushOutcome{retryable: true, delay: pushRetryDelay}
	}

	if status == http.StatusNotFound {
		// A server that predates relocks. Nothing the streamer can act
		// on, so it must not turn the tray amber. Deliberately NOT shared
		// with pushUnlockOnce: /steam/unlock has been deployed for ages,
		// so a 404 there is a real routing problem worth surfacing.
		logf("relock appid=%d http=404 — server does not accept relocks yet", appid)
		return pushOutcome{}
	}
	if status != http.StatusOK {
		label, blocked := classifyHTTPError(status, respBody)
		logf("relock appid=%d attempt=%d/%d http=%d code=%s msg=%q",
			appid, attempt, pushAttempts, status, orDash(parseAPIError(respBody).Code), label)
		if blocked {
			state.recordTransportBlock(label)
			return pushOutcome{}
		}
		state.recordSoftError(label)
		return pushOutcome{retryable: retryableStatus(status), delay: pushRetryDelay}
	}

	state.recordRequestOK()

	var decoded unlockResponse
	_ = json.Unmarshal(respBody, &decoded)
	logf("relock appid=%d count=%d attempt=%d/%d http=200 status=%s reason=%s tracker=%s",
		appid, len(apiNames), attempt, pushAttempts,
		orDash(decoded.Status), orDash(decoded.Reason), orDash(decoded.TrackerSlug))

	if relockRetryable(decoded.Status) {
		return pushOutcome{retryable: true, delay: pushRetryDelay}
	}
	return pushOutcome{}
}

// relockRetryable: only "busy" (the tracker was mid-push) can change on
// a retry. no_tracker is not a delivery failure worth painting the tray
// for — a reset between runs with no overlay open is ordinary, and the
// server has already made the achievements announceable again.
func relockRetryable(status string) bool {
	return status == "busy"
}

// parseUnlockResponse never fails: an unparseable or empty body from an
// older server degrades to a zero Status, which classifyUnlock reads as
// the legacy "200 means delivered" contract.
func parseUnlockResponse(body []byte) unlockResponse {
	var decoded unlockResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		preview := strings.TrimSpace(string(body))
		logf("unlock: unparseable 200 body=%q — treating as delivered (pre-guard server)", truncate(preview, 200))
	}
	return decoded
}

// classifyUnlock maps an API status to a dot colour and a label a
// streamer can act on. Unknown statuses and a missing reason both
// degrade to a truthful generic message rather than to a raw code or a
// crash — production has not shipped `reason` yet.
func classifyUnlock(status, reason string) (healthLevel, string) {
	switch status {
	case "injected", "already_unlocked":
		return healthGreen, ""

	case "":
		// Pre-guard server: a 200 with no status is the old "accepted"
		// contract. Do not paint it amber — that would flag every
		// healthy client talking to today's production.
		return healthGreen, ""

	case "no_tracker":
		switch reason {
		case "no_active_tracker":
			return healthAmber, "no StreamTrackr overlay is open"
		case "other_platform":
			return healthAmber, "your StreamTrackr tracker is set to a platform other than Steam"
		case "other_game":
			return healthAmber, "your StreamTrackr tracker is following a different game"
		default:
			// reason absent (current production) or a value added
			// server-side after this build shipped.
			return healthAmber, "no StreamTrackr tracker can receive this achievement"
		}

	case "no_baseline":
		return healthAmber, "your StreamTrackr overlay is still starting up — retrying"

	case "unknown_achievement":
		return healthAmber, "StreamTrackr doesn't know this achievement yet"

	default:
		return healthAmber, "StreamTrackr couldn't use this achievement (" + truncate(status, 40) + ")"
	}
}

// classifyHTTPError turns a non-2xx into a user-facing label and says
// whether it is a hard block (stop hammering, go red) or a transient
// one. blocked=true for 401/403 only: everything else can plausibly fix
// itself.
func classifyHTTPError(status int, body []byte) (label string, blocked bool) {
	e := parseAPIError(body)
	switch status {
	case http.StatusUnauthorized:
		return "your session expired — click “Sign in again”", true
	case http.StatusForbidden:
		switch e.Code {
		case "PRO_LICENSE_EXPIRED":
			return "your Pro subscription has expired", true
		case "PRO_LICENSE_REQUIRED":
			return "this needs an active StreamTrackr Pro subscription", true
		default:
			// Code absent — the state of production today.
			return "your StreamTrackr Pro subscription isn't active", true
		}
	case http.StatusTooManyRequests:
		return "StreamTrackr is rate-limiting this companion", false
	}
	if status >= 500 {
		return fmt.Sprintf("StreamTrackr is having trouble (HTTP %d)", status), false
	}
	return fmt.Sprintf("StreamTrackr rejected the request (HTTP %d)", status), false
}

func parseAPIError(body []byte) apiError {
	var e apiError
	_ = json.Unmarshal(body, &e) // best effort: an unparseable body just yields empty fields
	return e
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type identityResponse struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

// fetchIdentity asks /api/companion/auth/me who owns this token.
// Returns empty strings + error on any failure; caller renders nothing.
func fetchIdentity(backend, token string) (string, string, error) {
	if token == "" {
		return "", "", fmt.Errorf("no token")
	}
	url := strings.TrimRight(backend, "/") + "/api/companion/auth/me"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logf("identity error=network msg=%q", err.Error())
		state.recordSoftError("network error reaching StreamTrackr")
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != 200 {
		label, blocked := classifyHTTPError(resp.StatusCode, body)
		logf("identity http=%d code=%s msg=%q", resp.StatusCode, orDash(parseAPIError(body).Code), label)
		if blocked {
			state.recordTransportBlock(label)
		}
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Counts as proof of life: this is a real round-trip the server
	// accepted, which is exactly what the watchdog measures.
	state.recordRequestOK()
	var decoded identityResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", err
	}
	return decoded.Email, decoded.DisplayName, nil
}

// revokeSelf asks the backend to mark this token as revoked.
// Best-effort — caller clears the local token regardless.
func revokeSelf(backend, token string) error {
	if token == "" {
		return nil
	}
	url := strings.TrimRight(backend, "/") + "/api/companion/auth/me"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// appID accepts both `346900` and `"346900"` because the Node `steamapi`
// package serialises gameID as a string and bubbles it through
// /api/companion/current-game.
type appID uint32

func (a *appID) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*a = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return fmt.Errorf("appId %q: %w", s, err)
	}
	*a = appID(n)
	return nil
}

type currentGameResponse struct {
	AppID appID  `json:"appId"`
	Name  string `json:"name,omitempty"`
}

// pollCurrentGame doubles as the backend-side game fallback and as the
// heartbeat that proves this companion is still talking to the API.
// Every exit path now records what happened, so "is this client sending
// anything?" is answerable from a user's log — it wasn't before.
func pollCurrentGame(backend, token string) (uint32, string) {
	if token == "" {
		// The signature of the lost-token bug: green dot, zero requests.
		// It cost weeks of blind debugging by being silent here.
		logf("current-game skipped=no-token")
		return 0, ""
	}
	url := strings.TrimRight(backend, "/") + "/api/companion/current-game"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		logf("current-game error=request msg=%q", err.Error())
		return 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logf("current-game error=network msg=%q", err.Error())
		state.recordSoftError("network error reaching StreamTrackr")
		return 0, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	if resp.StatusCode != 200 {
		label, blocked := classifyHTTPError(resp.StatusCode, body)
		logf("current-game http=%d code=%s msg=%q", resp.StatusCode, orDash(parseAPIError(body).Code), label)
		if blocked {
			state.recordTransportBlock(label)
		} else {
			state.recordSoftError(label)
		}
		return 0, ""
	}
	state.recordRequestOK()

	var decoded currentGameResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		// Log body preview so a future schema drift is one log tail away.
		logf("current-game http=200 error=parse msg=%q body=%q", err.Error(), truncate(string(body), 256))
		return 0, ""
	}
	logf("current-game http=200 appid=%d", uint32(decoded.AppID))
	return uint32(decoded.AppID), decoded.Name
}
