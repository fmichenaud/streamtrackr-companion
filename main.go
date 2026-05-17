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
	setToken(*tokenF)
	if *tokenF != "" {
		logf("  backend : %s (push enabled)", *backendF)
		state.setAuthenticated(true)
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
	setToken(token)

	state.setMode("auto")
	state.setAuthenticated(token != "")

	if token == "" {
		token = ensureAuthenticated(backend, done)
		if token == "" {
			waitForTokenOrDone(&token, done)
			if token == "" {
				return
			}
		}
		setToken(token)
		state.setAuthenticated(true)
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
	if !showWelcomeDialog() {
		logf("welcome: user dismissed — waiting for a manual re-pair")
		return ""
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
func setToken(v string)   { tokenFlag.Store(&v) }

// runAutoMode reads RunningAppID, hands the appid to runForGame, and
// loops on game changes. Backend /current-game is a fallback when the
// registry read fails; pinged once a minute regardless to keep the
// server-side companion heartbeat alive. Token + backend are read from
// the atomic globals on every call so a mid-session re-login or logout
// from the tray menu takes effect immediately.
func runAutoMode(detectInterval, poll time.Duration, done <-chan struct{}) {
	logf("%s waiting for a Steam game…", stamp())

	const heartbeatInterval = 60 * time.Second
	lastHeartbeat := time.Time{}
	pingHeartbeat := func() {
		if time.Since(lastHeartbeat) < heartbeatInterval {
			return
		}
		go pollCurrentGame(currentBackend(), currentToken())
		lastHeartbeat = time.Now()
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
			logf("local detect failed (%v) — falling back to /current-game", localErr)
			appid, name = pollCurrentGame(currentBackend(), currentToken())
			lastHeartbeat = time.Now()
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
		isStillCurrent := func() bool {
			pingHeartbeat()
			cur, _, err := detectLocalGame()
			if err != nil {
				// Transient registry error — be optimistic, next tick retries.
				return true
			}
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
	steamPath, err := readSteamPath()
	if err != nil {
		logf("%s readSteamPath: %v — can't watch achievements without a Steam install", stamp(), err)
		return waitUntilDoneOrInactive(done, isStillCurrent)
	}
	steamID3, err := readCurrentSteamID3(steamPath)
	if err != nil {
		logf("%s readCurrentSteamID3: %v — Steam hasn't been logged in?", stamp(), err)
		return waitUntilDoneOrInactive(done, isStillCurrent)
	}

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
			return waitUntilDoneOrInactive(done, isStillCurrent)
		}
	}
	if len(slots) == 0 {
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
	logf("%s steamID3=%d (steamID64=%d)", stamp(), steamID3, steamID64FromAccountID(steamID3))

	statsPath := statsCachePath(steamPath, steamID3, appid)
	logf("%s watching %s", stamp(), statsPath)

	// Only push 0→1 transitions. Re-locks (SAM can flip bits back for
	// testing) are dev-tool noise, not player events.
	prev := baseline

	rescan := func() {
		stats, err := readUserStats(steamPath, steamID3, appid)
		if err != nil {
			logf("%s    rescan stats: %v", stamp(), err)
			return
		}
		next := computeUnlocked(slots, stats)
		for apiName, isUnlocked := range next {
			if isUnlocked && !prev[apiName] {
				logf("%s 🏆 UNLOCKED %s", stamp(), apiName)
				state.recordUnlock(apiName)
				// Read token+backend fresh so a mid-session re-login
				// from the tray takes effect on the very next push.
				// Display name is resolved server-side from the Web API.
				go pushUnlock(currentBackend(), currentToken(), appid, apiName, "")
			}
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
				continue
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

// waitUntilDoneOrInactive blocks until done closes or isStillCurrent
// goes false. Used by runForGame's early-exit paths.
func waitUntilDoneOrInactive(done <-chan struct{}, isStillCurrent func() bool) error {
	if isStillCurrent == nil {
		if done == nil {
			select {}
		}
		<-done
		return nil
	}
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return nil
		case <-t.C:
			if !isStillCurrent() {
				return nil
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

	steamID3, err := readCurrentSteamID3(steamPath)
	if err != nil {
		log.Fatalf("dump-stats: readCurrentSteamID3: %v", err)
	}

	fmt.Printf("steam-path : %s\n", steamPath)
	fmt.Printf("steamID3   : %d (steamID64 %d)\n", steamID3, steamID64FromAccountID(steamID3))
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

