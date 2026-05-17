package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// pushUnlock POSTs an unlock event to the backend. Blocks up to 5 s on
// the network so it never stalls the watcher; fire-and-forget on
// failure (logged + surfaced via state.recordPush).
func pushUnlock(backend, token string, appid uint32, apiName, displayName string) {
	if token == "" {
		// Diagnostic mode — no token, no push. Local detection still prints.
		return
	}
	body, err := json.Marshal(unlockPayload{
		AppID: appid,
		Achievement: achievementInfo{
			APIName:     apiName,
			DisplayName: displayName,
		},
	})
	if err != nil {
		logf("%s    push marshal: %v", stamp(), err)
		state.recordPush(false, err.Error())
		return
	}

	url := strings.TrimRight(backend, "/") + "/api/companion/steam/unlock"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		logf("%s    push build req: %v", stamp(), err)
		state.recordPush(false, err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logf("%s    ↗ push network error: %v", stamp(), err)
		state.recordPush(false, "network: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch resp.StatusCode {
	case 200:
		logf("%s    ↗ %s", stamp(), strings.TrimSpace(string(respBody)))
		state.recordPush(true, "")
	case 401:
		logf("%s    ↗ push 401 — token invalid or expired", stamp())
		state.recordPush(false, "invalid token — click 'Sign in again'")
	case 403:
		logf("%s    ↗ push 403 — not a Pro account", stamp())
		state.recordPush(false, "not a Pro account")
	default:
		errBody := strings.TrimSpace(string(respBody))
		logf("%s    ↗ push HTTP %d: %s", stamp(), resp.StatusCode, errBody)
		state.recordPush(false, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
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
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
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

// pollCurrentGame is the backend-side fallback when local registry
// detection fails. Returns appid=0 on no game / failure (auto-detect
// treats both the same).
func pollCurrentGame(backend, token string) (uint32, string) {
	if token == "" {
		return 0, ""
	}
	url := strings.TrimRight(backend, "/") + "/api/companion/current-game"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logf("pollCurrentGame: network error: %v", err)
		return 0, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		logf("pollCurrentGame: HTTP %d", resp.StatusCode)
		return 0, ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	var decoded currentGameResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		// Log body preview so a future schema drift is one log tail away.
		preview := string(body)
		if len(preview) > 256 {
			preview = preview[:256] + "…"
		}
		logf("pollCurrentGame: parse error: %v — body=%q", err, preview)
		return 0, ""
	}
	return uint32(decoded.AppID), decoded.Name
}
