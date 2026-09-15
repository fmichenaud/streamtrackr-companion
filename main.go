// StreamTrackr companion: tray-by-default, CLI subcommands available
// via login/logout/--cli/--dump-stats/--dump-kv. Tray reads config
// from disk + env vars; CLI mode reads flags.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"sync/atomic"
	"time"
)

// Default endpoints — overridable via flags or env vars.
const (
	defaultBackend  = "https://api.streamtrackr.com"
	defaultFrontend = "https://streamtrackr.com"
)

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func defaultLabel() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return "StreamTrackr Companion (" + host + ")"
}

func main() {
	// Sub-command dispatch. Each CLI branch calls ensureConsole() since
	// we ship with -H=windowsgui (no auto-allocated console).
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "login":
			ensureConsole()
			_ = initLogger(true)
			loginCmd(os.Args[2:])
			return
		case "logout":
			ensureConsole()
			_ = initLogger(true)
			runLogout()
			return
		case "-h", "--help", "help":
			ensureConsole()
			printUsage()
			return
		case "--cli", "-cli":
			os.Args = append(os.Args[:1], os.Args[2:]...)
			ensureConsole()
			_ = initLogger(true)
			runWatcher()
			return
		case "--dump-kv":
			ensureConsole()
			_ = initLogger(true)
			runDumpKV(os.Args[2:])
			return
		case "--dump-stats":
			ensureConsole()
			_ = initLogger(true)
			runDumpStats(os.Args[2:])
			return
		}
	}

	_ = initLogger(false)
	runTray()
}

func loginCmd(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	backend := fs.String("backend", envOr("STREAMTRACKR_BACKEND", defaultBackend), "StreamTrackr backend base URL")
	frontend := fs.String("frontend", envOr("STREAMTRACKR_FRONTEND", defaultFrontend), "StreamTrackr frontend (dashboard) base URL")
	label := fs.String("label", defaultLabel(), "Friendly label shown in the dashboard's paired-devices list")
	_ = fs.Parse(args)
	runLogin(*backend, *frontend, *label)
}

func printUsage() {
	fmt.Println(`StreamTrackr Companion

Default (no args, Windows):
    Opens the system tray and watches the running Steam game.

Sub-commands:
    streamtrackr-companion login   [-backend URL] [-frontend URL] [-label NAME]
    streamtrackr-companion logout
    streamtrackr-companion --cli [-appid N] [-poll DUR] [-backend URL] [-token T]
        Runs the watcher with a visible console — useful for dev / debug.
    streamtrackr-companion --dump-stats <appid> [-steam-path PATH]
        Dumps the parsed schema + current unlock state for a given
        app, by reading Steam's local appcache directly. Use this to
        verify the parser works against your Steam install before
        pairing the tray.`)
}

// ─────────────────────────── Watcher: timing helpers ──────────────────────────

var start = time.Now()

func stamp() string {
	d := time.Since(start)
	return fmt.Sprintf("T+%02d:%02d.%03d", int(d.Minutes())%60, int(d.Seconds())%60, d.Milliseconds()%1000)
}

// ────────────────────────────── CLI watcher ────────────────────────────────

func runWatcher() {
	storedToken, storedBackend, _ := loadToken()
	defaultBackendForFlag := storedBackend
	if defaultBackendForFlag == "" {
		defaultBackendForFlag = envOr("STREAMTRACKR_BACKEND", defaultBackend)
	}

	appidF := flag.Uint("appid", 0, "Force a specific Steam appid (skips auto-detect)")
	pollF := flag.Duration("poll", 250*time.Millisecond, "Stats-file polling interval")
	tokenF := flag.String("token", envOr("STREAMTRACKR_TOKEN", storedToken), "Bearer companion token")
	backendF := flag.String("backend", defaultBackendForFlag, "StreamTrackr backend base URL")
	detectF := flag.Duration("detect", 5*time.Second, "Auto-detect poll interval")
	flag.Parse()

	logf("StreamTrackr Steam companion — CLI mode")
	logf("  os      : %s/%s", runtime.GOOS, runtime.GOARCH)
	logf("  poll    : %s", *pollF)
	logf("  pid     : %d", os.Getpid())
	// Push runForGame / runAutoMode through the same atomic globals the
	// tray uses — keeps the rest of the code free of "is this CLI or
	// tray" branches and lets a tray-style re-auth (none in CLI today,
	// but cheap to support) work end-to-end.
	setBackend(*backendF)
	applyToken(*tokenF)
	if *tokenF != "" {
		logf("  backend : %s (push enabled)", *backendF)
		// The watchdog matters in CLI mode too — that's where support
		// sessions happen, and "health: green → red" in the log is the
		// fastest answer to "is this client still sending?".
		go runHealthMonitor(nil)
	} else {
		logf("  backend : -- (diagnostic mode, no HTTP push)")
	}

	if *appidF != 0 {
		state.setMode("manual")
		logf("  mode    : manual (-appid %d)", *appidF)
		_ = runForGame(uint32(*appidF), *pollF, nil, nil)
		return
	}

	if *tokenF == "" {
		log.Fatalf(`Auto-detect requires authentication.
Pair this companion first:

  streamtrackr-companion login

Or run in manual mode:

  streamtrackr-companion --cli -appid <N>`)
	}

	state.setMode("auto")
	logf("  mode    : auto-detect (poll backend every %s)", *detectF)
	runAutoMode(*detectF, *pollF, nil)
}

// ────────────────────────────── Tray watcher ───────────────────────────────

// runWatcherLoopForTray is the tray-mode entry point. Reads config
// from disk + env (no CLI flags), triggers OAuth pairing on first
// launch, runs the auto-detect loop until done is closed.
func runWatcherLoopForTray(done <-chan struct{}) {
	storedToken, storedBackend, _ := loadToken()
	backend := storedBackend
	if backend == "" {
		backend = envOr("STREAMTRACKR_BACKEND", defaultBackend)
	}
	token := envOr("STREAMTRACKR_TOKEN", storedToken)

	setBackend(backend)
	applyToken(token)

	state.setMode("auto")

	if token == "" {
		token = ensureAuthenticated(backend, done)
		if token == "" {
			waitForTokenOrDone(&token, done)
			if token == "" {
				return
			}
		}
		applyToken(token)
	}

	if s := state.snapshot(); s.UserEmail == "" && s.UserDisplayName == "" {
		go refreshIdentity(backend, token)
	}

	runAutoMode(5*time.Second, 250*time.Millisecond, done)
}

// ensureAuthenticated shows the welcome dialog, runs the loopback OAuth
// pairing, and returns the freshly minted token (or "" on refusal /
// timeout). done is honoured at every blocking step.
func ensureAuthenticated(backend string, done <-chan struct{}) string {
	// Show the welcome dialog at most once per process. The watcher is
	// supervised now, so a panic-restart must not pop a dialog every
	// few seconds.
	if !welcomeShown.Swap(true) {
		if !showWelcomeDialog() {
			logf("welcome: user dismissed — waiting for a manual re-pair")
			return ""
		}
	}
	frontend := envOr("STREAMTRACKR_FRONTEND", defaultFrontend)
	// Same convention as deriveFrontendURL — api.X.com → X.com — when
	// the user runs against a custom backend.
	if v := stripAPISubdomain(backend); v != "" {
		frontend = v
	}

	loginDone := make(chan error, 1)
	go func() { loginDone <- runLoginE(backend, frontend, defaultLabel()) }()

	// Poll the on-disk token alongside loginDone so we still pick up a
	// concurrent "Sign in again" click from the tray menu — without
	// this poll, two parallel runLoginE goroutines would deadlock here
	// for the full 10 min timeout.
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()

	for {
		select {
		case <-done:
			return ""
		case err := <-loginDone:
			if err != nil {
				logf("login: %v", err)
			}
			if tok, _, _ := loadToken(); tok != "" {
				go refreshIdentity(backend, tok)
				return tok
			}
			return ""
		case <-poll.C:
			if tok, _, _ := loadToken(); tok != "" {
				go refreshIdentity(backend, tok)
				return tok
			}
		}
	}
}

func refreshIdentity(backend, token string) {
	email, displayName, err := fetchIdentity(backend, token)
	if err != nil {
		logf("identity lookup failed: %v", err)
		return
	}
	state.setIdentity(email, displayName)
}

func waitForTokenOrDone(out *string, done <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if tok, _, _ := loadToken(); tok != "" {
				*out = tok
				return
			}
		}
	}
}

// stripAPISubdomain rewrites https://api.X.com → https://X.com so the
// pairing flow doesn't need a separate -frontend flag for the common
// case. Returns "" if the backend doesn't follow that convention.
func stripAPISubdomain(backend string) string {
	for _, prefix := range []string{"https://api.", "http://api."} {
		if len(backend) > len(prefix) && backend[:len(prefix)] == prefix {
			scheme := prefix[:len(prefix)-4]
			return scheme + backend[len(prefix):]
		}
	}
	return ""
}

// Tray-mode globals. Written from multiple goroutines (sign-in,
// sign-out, re-pair) hence atomic.Pointer. CLI mode doesn't touch
// these.
var (
	backendFlag atomic.Pointer[string]
	tokenFlag   atomic.Pointer[string]

	// welcomeShown guards the first-run dialog against the supervisor
	// restarting the watcher.
	welcomeShown atomic.Bool
)

func currentBackend() string {
	if p := backendFlag.Load(); p != nil {
		return *p
	}
	return ""
}

func currentToken() string {
	if p := tokenFlag.Load(); p != nil {
		return *p
	}
	return ""
}

func setBackend(v string) { backendFlag.Store(&v) }

// applyToken is the ONLY way to change the token. It moves the
// in-memory token and the flag the UI reads together, because letting
// them drift apart is precisely what broke: a re-pair updated the UI
// flag and not the token, so the tray showed a signed-in companion
// while every request path short-circuited on an empty token. The
// result was a green dot and total silence — no requests, no log lines,
// for as long as the app stayed open.
func applyToken(v string) {
	tokenFlag.Store(&v)
	state.setAuthenticated(v != "")
}

// Heartbeat cadence. A companion that is being refused (401/403) must
// not keep asking once a minute forever — that was 1 500 to 3 000
// pointless requests a day per blocked client, and it told the user
// nothing. Back off exponentially, cap at 15 min, and let the dot and
// the log carry the message instead.
const (
	heartbeatInterval    = 60 * time.Second
	heartbeatMaxInterval = 15 * time.Minute
	heartbeatMaxShift    = 8
)

func heartbeatBackoff(consecutiveBlocks int) time.Duration {
	if consecutiveBlocks <= 0 {
		return heartbeatInterval
	}
	if consecutiveBlocks > heartbeatMaxShift {
		consecutiveBlocks = heartbeatMaxShift
	}
	d := heartbeatInterval << uint(consecutiveBlocks)
	if d > heartbeatMaxInterval {
		return heartbeatMaxInterval
	}
	return d
}

func heartbeatDue(lastAttempt time.Time) bool {
	return time.Since(lastAttempt) >= heartbeatBackoff(state.snapshot().TransportBlocks)
}

// runAutoMode reads RunningAppID, hands the appid to runForGame, and
// loops on game changes. Backend /current-game is a fallback when the
// registry read fails; it is also pinged once a minute regardless (less
// often when the API is refusing us — see heartbeatBackoff) to keep the
// server-side companion heartbeat alive and to prove to the watchdog
// that this client is still talking. Token + backend are read from the
// atomic globals on every call so a mid-session re-login or logout from
// the tray menu takes effect immediately.
func runAutoMode(detectInterval, poll time.Duration, done <-chan struct{}) {
	logf("%s waiting for a Steam game…", stamp())

	var (
		lastHeartbeatAttempt time.Time
		heartbeatInFlight    atomic.Bool
	)
	// lastHeartbeatAttempt throttles *sending*; it says nothing about
	// whether anyone answered. Liveness lives in the shared state, fed
	// by pollCurrentGame itself, and is judged by runHealthMonitor on a
	// goroutine this loop can't take down with it.
	pingHeartbeat := func() {
		if !heartbeatDue(lastHeartbeatAttempt) {
			return
		}
		// One in flight at a time: a stalled request must not queue up
		// behind itself while the 5 s timeout runs down.
		if !heartbeatInFlight.CompareAndSwap(false, true) {
			return
		}
		lastHeartbeatAttempt = time.Now()
		go func() {
			defer heartbeatInFlight.Store(false)
			pollCurrentGame(currentBackend(), currentToken())
		}()
	}

	for {
		// Honour cancellation before the blocking detect call so quit
		// during a hung HTTP fallback doesn't wait the full timeout.
		if done != nil {
			select {
			case <-done:
				return
			default:
			}
		}

		appid, name, localErr := detectLocalGame()
		if localErr != nil {
			// The backend fallback used to fire on every 5 s tick here,
			// which is how an unreadable registry (or a permanent 403)
			// turned into a request storm. Rate-limit it like the
			// heartbeat, and keep the last known game while throttled
			// rather than flapping the UI to "no game".
			if !heartbeatDue(lastHeartbeatAttempt) {
				select {
				case <-done:
					return
				case <-time.After(detectInterval):
					continue
				}
			}
			logf("%s local detect failed (%v) — falling back to /current-game", stamp(), localErr)
			lastHeartbeatAttempt = time.Now()
			appid, name = pollCurrentGame(currentBackend(), currentToken())
		} else {
			pingHeartbeat()
		}

		if appid == 0 {
			state.setGame(0, "", 0, 0)
			select {
			case <-done:
				return
			case <-time.After(detectInterval):
				continue
			}
		}
		logf("%s ▶ game detected: %s (appid %d)", stamp(), name, appid)
		state.setGame(appid, name, 0, 0)

		sessionAppid := appid
		registryErrors := 0
		isStillCurrent := func() bool {
			pingHeartbeat()
			cur, _, err := detectLocalGame()
			if err != nil {
				// Transient registry error — be optimistic, next tick
				// retries. But "optimistic forever" is how a session
				// outlives its game, so say something once it stops
				// looking transient (~1 min at the 4 s alive tick).
				registryErrors++
				if registryErrors == 15 {
					logf("%s registry unreadable for ~1 min (%v) — still assuming appid %d is running", stamp(), err, sessionAppid)
				}
				return true
			}
			registryErrors = 0
			return cur == sessionAppid
		}
		_ = runForGame(appid, poll, isStillCurrent, done)
		logf("%s ◼ session ended for appid %d", stamp(), appid)

		// Drain a short delay before the next detect cycle, respecting cancellation.
		select {
		case <-done:
			return
		case <-time.After(detectInterval):
		}
	}
}

// How long to wait before re-reading the stats file to confirm a relock.
// Long enough for Steam to finish an in-place rewrite, short enough that
// a real reset still reaches the overlay while the player is still
// looking at it.
const relockConfirmDelay = 200 * time.Millisecond

// runForGame reads the schema + stats baseline, then polls the stats
// file's mtime for new unlocks. Returns when done closes or
// isStillCurrent goes false. Pre-loop errors are non-fatal — log and
// park until the session ends. Token + backend are read fresh from the
// atomic globals on each push so re-login from the tray takes effect
// without waiting for the session to end.
func runForGame(
	appid uint32,
	poll time.Duration,
	isStillCurrent func() bool,
	done <-chan struct{},
) error {
	// Every early exit below leaves the session running while watching
	// nothing. Each one used to be a log line and a green dot; they are
	// now in the state, because "no achievement will ever be detected"
	// is exactly as fatal to the user as a 403.
	defer state.clearCaptureError()

	steamPath, err := readSteamPath()
	if err != nil {
		logf("%s readSteamPath: %v — can't watch achievements without a Steam install", stamp(), err)
		state.setCaptureError("can't find your Steam install — achievements aren't being watched")
		return waitUntilDoneOrInactive(done, isStillCurrent)
	}
	steamID3, idSource, ok := resolveSteamID3Blocking(steamPath, done, isStillCurrent)
	if !ok {
		return nil
	}
	state.clearCaptureError()

	// Schema file may arrive a few seconds after game launch if Steam
	// hasn't cached it yet — retry once before giving up.
	slots, err := readSchema(steamPath, appid)
	if err != nil {
		logf("%s readSchema(%d): %v — retrying in 3s", stamp(), appid, err)
		select {
		case <-done:
			return nil
		case <-time.After(3 * time.Second):
		}
		slots, err = readSchema(steamPath, appid)
		if err != nil {
			logf("%s readSchema(%d) retry failed: %v", stamp(), appid, err)
			state.setCaptureError("Steam hasn't cached this game's achievements — restart the game to fix")
			return waitUntilDoneOrInactive(done, isStillCurrent)
		}
	}
	if len(slots) == 0 {
		// Not a failure: plenty of games ship without achievements.
		logf("%s appid %d has no achievements in schema", stamp(), appid)
		return waitUntilDoneOrInactive(done, isStillCurrent)
	}

	stats, err := readUserStats(steamPath, steamID3, appid)
	if err != nil {
		logf("%s readUserStats(%d): %v — treating as all-locked baseline", stamp(), appid, err)
		stats = map[uint32]int32{}
	}
	baseline := computeUnlocked(slots, stats)

	unlockedNow := uint32(0)
	for _, ok := range baseline {
		if ok {
			unlockedNow++
		}
	}
	total := uint32(len(baseline))
	current := state.snapshot()
	state.setGame(current.CurrentAppID, current.CurrentGameName, unlockedNow, total)
	logf("%s baseline: %d/%d unlocked.", stamp(), unlockedNow, total)
	logf("%s steamID3=%d (steamID64=%d) via %s", stamp(), steamID3, steamID64FromAccountID(steamID3), idSource)

	statsPath := statsCachePath(steamPath, steamID3, appid)
	logf("%s watching %s", stamp(), statsPath)
	// A missing stats file is normal before the first StoreStats, but
	// paired with a guessed account it's the fingerprint of having picked
	// the wrong one — say so in the log rather than sitting silent. Log
	// it either way: which account answered is the first thing support
	// needs, and the old `idSource != registry` condition hid the note
	// exactly when the account came from the source we trust most.
	statsFileMissing := false
	if _, err := os.Stat(statsPath); os.IsNotExist(err) {
		statsFileMissing = true
		logf("%s note: no stats file yet (account from %s) — expected if this game was never played on it", stamp(), idSource)
	}

	// Push 0→1 and 1→0 transitions. Re-locks used to be dropped as SAM
	// noise, but a speedrunner clearing achievements from the Steam
	// console between attempts produces exactly these — and without them
	// the overlay kept the previous run's count.
	prev := baseline

	// Closed once the latest relock push is finished. Unlock pushes wait
	// on the one current when they were detected, so an achievement
	// earned again right after a reset can't reach the server before the
	// reset does — it would be refused as already unlocked, then locked.
	relockSettled := make(chan struct{})
	close(relockSettled)

	// The other direction: an unlock push already retrying when the reset
	// happens would land AFTER the relock. Ordering it would not help —
	// the server would still announce a trophy the player just erased —
	// so the relock calls it off instead. Only the same achievement is a
	// conflict, so unrelated pushes keep going in parallel.
	inflight := newInflightUnlocks()

	readUnlocks := func() (map[string]bool, error) {
		stats, err := readExistingUserStats(steamPath, steamID3, appid)
		if err != nil {
			return nil, err
		}
		return computeUnlocked(slots, stats), nil
	}

	rescan := func() {
		next, err := readUnlocks()
		if err != nil {
			logf("%s    rescan stats: %v", stamp(), err)
			return
		}

		// A relock is the one transition worth reading twice. Steam
		// rewrites the stats file in place, so a partial write that still
		// parses reads as achievements going away — a whole game re-locked
		// on a hiccup, announced as a reset nobody asked for. The second
		// read costs 200 ms on the rare rescan that sees one, and nothing
		// at all on every other rescan.
		if len(relockedSince(prev, next)) > 0 {
			select {
			case <-done:
				return
			case <-time.After(relockConfirmDelay):
			}
			confirmed, err := readUnlocks()
			if err != nil {
				logf("%s    relock not confirmed (%v) — leaving this rescan for the next tick", stamp(), err)
				return
			}
			if !sameUnlocks(next, confirmed) {
				logf("%s    stats file was still settling — trusting the second read", stamp())
			}
			next = confirmed
		}

		if relocked := relockedSince(prev, next); len(relocked) > 0 {
			logf("%s ↺ RELOCKED %d achievement(s): %s", stamp(), len(relocked), joinNames(relocked, 10))
			if stopped := inflight.cancel(relocked); len(stopped) > 0 {
				logf("%s    called off %d unlock push(es) still retrying: %s", stamp(), len(stopped), joinNames(stopped, 10))
			}
			previous, settled := relockSettled, make(chan struct{})
			relockSettled = settled
			go func() {
				defer close(settled)
				select {
				case <-previous:
				case <-done:
					return
				}
				pushRelock(currentBackend(), currentToken(), appid, relocked, done)
			}()
		}

		for _, apiName := range unlockedSince(prev, next) {
			logf("%s 🏆 UNLOCKED %s", stamp(), apiName)
			state.recordUnlock(apiName)
			// Read token+backend fresh so a mid-session re-login
			// from the tray takes effect on the very next push.
			// Display name is resolved server-side from the Web API.
			cancel := inflight.start(apiName)
			go func(apiName string, after <-chan struct{}, cancel chan struct{}) {
				defer inflight.finish(apiName, cancel)
				select {
				case <-after:
				case <-cancel:
					return
				case <-done:
					return
				}
				pushUnlock(currentBackend(), currentToken(), appid, apiName, "", done, cancel)
			}(apiName, relockSettled, cancel)
		}
		prev = next
	}

	statsTick := time.NewTicker(poll)
	defer statsTick.Stop()
	aliveTick := time.NewTicker(4 * time.Second)
	defer aliveTick.Stop()

	var lastMod time.Time
	if fi, err := os.Stat(statsPath); err == nil {
		lastMod = fi.ModTime()
	}

	for {
		select {
		case <-done:
			return nil
		case <-statsTick.C:
			fi, err := os.Stat(statsPath)
			if err != nil {
				// Silent `continue` here means we can poll a file that
				// will never exist for the entire session and never say
				// so. Log the appear/disappear transitions — at 250 ms
				// a per-tick log would be unusable, but a transition is
				// one line and answers "was it ever there?".
				if !statsFileMissing {
					statsFileMissing = true
					logf("%s stats file went away (%v) — waiting for it to come back", stamp(), err)
				}
				continue
			}
			if statsFileMissing {
				statsFileMissing = false
				logf("%s stats file is there now — watching for unlocks", stamp())
			}
			if !fi.ModTime().Equal(lastMod) {
				lastMod = fi.ModTime()
				rescan()
			}
		case <-aliveTick.C:
			if isStillCurrent != nil && !isStillCurrent() {
				return nil
			}
		}
	}
}

// Steam can be mid-startup when a game launches (Big Picture, a shortcut
// that starts Steam itself), so the active account may not be readable
// for a few seconds. Retry for as long as the session lasts instead of
// giving up on it — a one-shot read used to leave the whole session
// unwatched: no stats file, no unlocks, no error visible to the user.
const steamID3RetryInterval = 10 * time.Second

// resolveSteamID3Blocking retries until an account resolves, the game
// stops being the current one, or done closes. ok=false means the caller
// should end the session quietly.
func resolveSteamID3Blocking(
	steamPath string,
	done <-chan struct{},
	isStillCurrent func() bool,
) (steamID3 uint32, source string, ok bool) {
	for attempt := 1; ; attempt++ {
		id, source, err := resolveSteamID3(steamPath)
		if err == nil {
			if attempt > 1 {
				logf("%s steamID3 resolved on attempt %d (%s)", stamp(), attempt, source)
			}
			return id, source, true
		}
		// First failure, then every ~5 min — enough to date the problem
		// in a support log without flooding it.
		if attempt == 1 || attempt%30 == 0 {
			logf("%s resolveSteamID3: %v — retrying every %s", stamp(), err, steamID3RetryInterval)
		}
		// Retrying quietly still means zero achievements detected for as
		// long as it lasts. Put it in front of the user instead of only
		// in the log.
		state.setCaptureError("can't tell which Steam account is signed in — achievements aren't being watched")

		select {
		case <-done:
			return 0, "", false
		case <-time.After(steamID3RetryInterval):
		}
		if isStillCurrent != nil && !isStillCurrent() {
			return 0, "", false
		}
	}
}

// waitUntilDoneOrInactive blocks until done closes or isStillCurrent
// goes false. Used by runForGame's early-exit paths.
func waitUntilDoneOrInactive(done <-chan struct{}, isStillCurrent func() bool) error {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	// Parked sessions are the quietest failure mode in the app: nothing
	// is being watched and nothing says so. The state carries the
	// reason (see the capture errors above); this reminder dates it in
	// the log every 5 min so a support tail can't mistake a parked
	// session for a working one.
	const remindEvery = 5 * time.Minute
	lastReminder := time.Now()

	for {
		select {
		case <-done:
			return nil
		case <-t.C:
			if isStillCurrent != nil && !isStillCurrent() {
				return nil
			}
			if time.Since(lastReminder) >= remindEvery {
				lastReminder = time.Now()
				if capture := state.snapshot().CaptureError; capture != "" {
					logf("%s still parked: %s", stamp(), capture)
				}
			}
		}
	}
}

// runDumpKV pretty-prints the BinaryKV tree of any Steam cache file.
func runDumpKV(args []string) {
	if len(args) < 1 {
		log.Fatalf("usage: streamtrackr-companion --dump-kv <file>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		log.Fatalf("dump-kv: read: %v", err)
	}
	root, err := parseBinaryKV(data)
	if err != nil {
		log.Fatalf("dump-kv: parse: %v", err)
	}
	printKV(root, 0)
}

func printKV(n *kvNode, depth int) {
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "  "
	}
	switch n.Type {
	case kvNone:
		fmt.Printf("%s%q {\n", indent, n.Name)
		for _, c := range n.Children {
			printKV(c, depth+1)
		}
		fmt.Printf("%s}\n", indent)
	case kvString, kvWStr:
		fmt.Printf("%s%q = %q\n", indent, n.Name, n.Str)
	case kvFloat:
		fmt.Printf("%s%q = %g (float)\n", indent, n.Name, n.Float)
	default:
		fmt.Printf("%s%q = %d (type=0x%02X)\n", indent, n.Name, n.Int, n.Type)
	}
}

// runDumpStats prints the schema + current unlock state for one appid.
func runDumpStats(args []string) {
	fs := flag.NewFlagSet("dump-stats", flag.ExitOnError)
	steamPathFlag := fs.String("steam-path", "", "Steam install directory (defaults to registry lookup)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("dump-stats: %v", err)
	}
	if fs.NArg() < 1 {
		log.Fatalf("usage: streamtrackr-companion --dump-stats <appid> [-steam-path PATH]")
	}
	var appid uint32
	if _, err := fmt.Sscanf(fs.Arg(0), "%d", &appid); err != nil || appid == 0 {
		log.Fatalf("dump-stats: invalid appid %q", fs.Arg(0))
	}

	steamPath := *steamPathFlag
	if steamPath == "" {
		var err error
		steamPath, err = readSteamPath()
		if err != nil {
			log.Fatalf("dump-stats: readSteamPath: %v\n\nPass -steam-path manually if you're not on Windows.", err)
		}
	}

	steamID3, idSource, err := resolveSteamID3(steamPath)
	if err != nil {
		log.Fatalf("dump-stats: resolveSteamID3: %v", err)
	}

	fmt.Printf("steam-path : %s\n", steamPath)
	fmt.Printf("steamID3   : %d (steamID64 %d) via %s\n", steamID3, steamID64FromAccountID(steamID3), idSource)
	fmt.Printf("appid      : %d\n", appid)
	fmt.Printf("schema     : %s\n", schemaCachePath(steamPath, appid))
	fmt.Printf("stats      : %s\n\n", statsCachePath(steamPath, steamID3, appid))

	slots, err := readSchema(steamPath, appid)
	if err != nil {
		log.Fatalf("dump-stats: readSchema: %v", err)
	}
	if len(slots) == 0 {
		fmt.Println("(schema parsed OK but no achievements found — game may not have any)")
		return
	}

	stats, err := readUserStats(steamPath, steamID3, appid)
	if err != nil {
		log.Fatalf("dump-stats: readUserStats: %v", err)
	}
	unlocked := computeUnlocked(slots, stats)

	sort.Slice(slots, func(i, j int) bool {
		if slots[i].StatID != slots[j].StatID {
			return slots[i].StatID < slots[j].StatID
		}
		return slots[i].Bit < slots[j].Bit
	})

	gotCount := 0
	for _, s := range slots {
		marker := "  "
		if unlocked[s.APIName] {
			marker = "🏆"
			gotCount++
		}
		fmt.Printf("%s  stat=%-6d bit=%-2d  %s\n", marker, s.StatID, s.Bit, s.APIName)
	}
	fmt.Printf("\nTotal: %d/%d unlocked\n", gotCount, len(slots))
}

