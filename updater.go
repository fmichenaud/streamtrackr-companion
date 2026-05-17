package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/inconshreveable/go-update"
)

// Set via -ldflags "-X main.version=…". "dev" disables the auto-updater
// so dev builds don't overwrite themselves with a real release.
var version = "dev"

const defaultManifestURL = "https://github.com/fmichenaud/streamtrackr-companion/releases/latest/download/manifest.json"

// allowedManifestHosts: github.com handles the /releases/latest/download
// redirect, objects.githubusercontent.com is where asset bytes land.
// Closed allowlist so a MITM can't redirect us to an unsigned binary
// even if the manifest URL ever gets attacker-influenced.
var allowedManifestHosts = map[string]bool{
	"github.com":                   true,
	"objects.githubusercontent.com": true,
}

// validateManifestURL enforces HTTPS + host allowlist on both the
// manifest URL and the binary URL embedded inside it.
func validateManifestURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("unparseable URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("scheme must be https, got %q", u.Scheme)
	}
	if !allowedManifestHosts[u.Host] {
		return fmt.Errorf("host %q not in allowlist", u.Host)
	}
	return nil
}

// manifestURL honours STREAMTRACKR_COMPANION_MANIFEST_URL when it
// validates, otherwise falls back to the baked-in default — never
// promote a malformed/off-allowlist env var to an update source.
func manifestURL() string {
	v := os.Getenv("STREAMTRACKR_COMPANION_MANIFEST_URL")
	if v == "" {
		return defaultManifestURL
	}
	if err := validateManifestURL(v); err != nil {
		logf("updater: STREAMTRACKR_COMPANION_MANIFEST_URL ignored (%v) — using default", err)
		return defaultManifestURL
	}
	return v
}

var updateAvailable int32

func updateReady() bool { return atomic.LoadInt32(&updateAvailable) == 1 }

type manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// startAutoUpdater polls the manifest URL every `interval` (default
// 6 h), plus once 30 s after start. Honours done at every blocking step.
func startAutoUpdater(interval time.Duration, done <-chan struct{}) {
	if version == "dev" {
		logf("updater: dev build — skipping auto-update loop.")
		return
	}
	go func() {
		// Initial poll after a short delay so the watcher loop has time
		// to attach to a running game before we hog the user's bandwidth
		// for a download.
		select {
		case <-done:
			return
		case <-time.After(30 * time.Second):
		}
		if err := checkOnce(); err != nil {
			logf("updater: %v", err)
		}

		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := checkOnce(); err != nil {
					logf("updater: %v", err)
				}
			}
		}
	}()
}

// fetchManifest returns (nil, nil) for empty/unparseable manifests so a
// GitHub hiccup doesn't crash the watcher.
func fetchManifest() (*manifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("manifest fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("manifest HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	if m.Version == "" || m.URL == "" || m.SHA256 == "" {
		return nil, nil
	}
	return &m, nil
}

// applyManifest downloads, SHA-256 verifies, and atomically replaces
// the binary, then flips updateAvailable.
func applyManifest(m *manifest) error {
	// Re-validate the URL inside the manifest — it's attacker-controlled
	// JSON even when the manifest itself came from a trusted host.
	if err := validateManifestURL(m.URL); err != nil {
		return fmt.Errorf("manifest URL rejected: %w", err)
	}
	logf("updater: new version %s available (current %s) — downloading…", m.Version, version)
	bin, err := downloadAndVerify(m.URL, m.SHA256)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := update.Apply(bytes.NewReader(bin), update.Options{}); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	atomic.StoreInt32(&updateAvailable, 1)
	logf("updater: %s installed — restart pending.", m.Version)
	return nil
}

// checkOnce is the silent fetch-and-apply path. Idempotent.
func checkOnce() error {
	if updateReady() {
		return nil // already applied; waiting for restart
	}
	m, err := fetchManifest()
	if err != nil {
		return err
	}
	if m == nil || !isNewer(m.Version, version) {
		return nil
	}
	return applyManifest(m)
}

// 50 MiB cap on update downloads — safety net against an unbounded
// stream eating memory if the response ever misbehaves.
const maxBinarySize = 50 * 1024 * 1024

// downloadAndVerify buffers the whole binary in memory so a SHA-256
// mismatch is caught before update.Apply touches disk.
func downloadAndVerify(url, expectedHex string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "streamtrackr-companion/"+version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	// +1 so a download exactly at the cap is distinguishable from overflow.
	limited := io.LimitReader(resp.Body, maxBinarySize+1)
	body, err := io.ReadAll(io.TeeReader(limited, h))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBinarySize {
		return nil, fmt.Errorf("download exceeded %d bytes — refusing to apply", maxBinarySize)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(expectedHex) {
		return nil, fmt.Errorf("sha256 mismatch (manifest=%s got=%s)", expectedHex, got)
	}
	return body, nil
}

// restartSelf relaunches the (now-updated) binary and exits.
func restartSelf() {
	exe, err := os.Executable()
	if err != nil {
		logf("restart: Executable(): %v", err)
		return
	}
	cmd := exec.Command(exe)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		logf("restart: Start(): %v", err)
		return
	}
	logf("restart: relaunched as pid=%d — exiting current process.", cmd.Process.Pid)
	os.Exit(0)
}

// isNewer compares plain semver triplets (no pre-release / metadata).
func isNewer(a, b string) bool {
	return compareVersion(a, b) > 0
}

func compareVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai := segmentInt(as, i)
		bi := segmentInt(bs, i)
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

func segmentInt(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	v, _ := strconv.Atoi(parts[i])
	return v
}
