package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// runLogin is the CLI entry — exits on failure. Tray code uses
// runLoginE directly so a failed pairing doesn't tear down the UI.
func runLogin(backend, frontend, label string) {
	if err := runLoginE(backend, frontend, label); err != nil {
		exitf("%v", err)
	}
}

// runLoginE runs the RFC 8252 loopback OAuth flow: random state nonce,
// local HTTP server on :0, browser to /companion/auth, exchange code
// at /api/companion/auth/exchange, persist token. Times out after 10 min.
func runLoginE(backend, frontend, label string) error {
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return fmt.Errorf("rand: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/cb", port)

	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/cb", makeCallbackHandler(state, resultCh))

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	authURL := buildAuthURL(frontend, redirectURI, state, label)
	logf("login: opening browser at %s", authURL)
	if err := openBrowser(authURL); err != nil {
		logf("login: openBrowser failed: %v (URL is in the log above)", err)
	}

	var result callbackResult
	select {
	case result = <-resultCh:
	case <-time.After(10 * time.Minute):
		return fmt.Errorf("timeout — re-run pairing from the tray menu")
	}

	if result.err != nil {
		return result.err
	}

	token, err := exchangeCode(backend, result.code, state, label)
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}

	if err := saveToken(token, backend); err != nil {
		return fmt.Errorf("save token: %w", err)
	}

	path, _ := tokenStorePath()
	logf("login: companion paired — token saved to %s", path)
	return nil
}

// runLogout removes the local token only. Server-side revoke is done
// via the dashboard.
func runLogout() {
	if err := clearToken(); err != nil {
		exitf("logout: %v", err)
	}
	fmt.Println("Local token removed. To revoke server-side, head to the dashboard → paired devices.")
}

type callbackResult struct {
	code string
	err  error
}

func makeCallbackHandler(expectedState string, ch chan<- callbackResult) http.HandlerFunc {
	// sync.Once: link prefetchers + double-clicks can hit /cb twice
	// concurrently; only one result must reach the channel.
	var once sync.Once
	deliver := func(r callbackResult) {
		once.Do(func() { ch <- r })
	}
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		state := q.Get("state")
		code := q.Get("code")
		errParam := q.Get("error")

		switch {
		case errParam != "":
			respondHTML(w, "Refused", "You declined the authorization. You can close this tab.")
			deliver(callbackResult{err: fmt.Errorf("authorization refused by user")})
		case state != expectedState:
			respondHTML(w, "Invalid state", "The `state` parameter doesn't match — possible CSRF attack. Restart authorization from the companion.")
			deliver(callbackResult{err: fmt.Errorf("state mismatch (CSRF guard)")})
		case code == "":
			respondHTML(w, "Error", "No code received. Restart authorization.")
			deliver(callbackResult{err: fmt.Errorf("no code in callback")})
		default:
			respondHTML(w, "Companion authorized", "You can close this tab — the companion has taken over.")
			deliver(callbackResult{code: code})
		}
	}
}

func respondHTML(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="fr">
<head><meta charset="utf-8"><title>StreamTrackr — %s</title>
<style>body{font-family:system-ui,-apple-system,sans-serif;background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0}.card{max-width:480px;padding:2rem;background:#1e293b;border-radius:1rem;text-align:center}h1{margin-top:0;color:#fff}p{color:#94a3b8;line-height:1.6}</style>
</head><body><div class="card"><h1>%s</h1><p>%s</p></div></body></html>`,
		htmlEscape(title), htmlEscape(title), htmlEscape(body))
}

func buildAuthURL(frontend, redirectURI, state, label string) string {
	u, _ := url.Parse(strings.TrimRight(frontend, "/") + "/companion/auth")
	q := u.Query()
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	if label != "" {
		q.Set("label", label)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// exchangeCode redeems the one-time auth code for a long-lived bearer.
// Body shape matches api-nestjs/src/companion/companion-auth.controller.ts.
func exchangeCode(backend, code, state, label string) (string, error) {
	payload := map[string]string{"code": code, "state": state}
	if label != "" {
		payload["label"] = label
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(backend, "/")+"/api/companion/auth/exchange",
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("network: %w", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		Token     string `json:"token"`
		TokenId   string `json:"tokenId"`
		ExpiresAt string `json:"expiresAt"`
		Message   string `json:"message"`
	}
	// Cap response body — backend URL is user-controlled (flag + env),
	// so a rogue or MITM-downgraded endpoint could otherwise OOM us.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("HTTP %d: non-JSON response", resp.StatusCode)
	}
	if resp.StatusCode != 200 || decoded.Token == "" {
		msg := decoded.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("%s", msg)
	}
	return decoded.Token, nil
}

// openBrowser is best-effort — if it fails the user can paste the URL.
func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Erreur: "+format+"\n", args...)
	os.Exit(1)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return r.Replace(s)
}
