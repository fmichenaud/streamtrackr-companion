//go:build windows

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

//go:embed assets/icon.ico
var trayIcon []byte

// runTray launches the systray UI. systray.Run blocks until quit;
// onTrayReady spawns the watcher + refresh + menu-event goroutines.
func runTray() {
	systray.Run(onTrayReady, onTrayExit)
}

var (
	itemStatus     *systray.MenuItem
	itemAccount    *systray.MenuItem
	itemGame       *systray.MenuItem
	itemLastUnlock *systray.MenuItem
	itemRestart    *systray.MenuItem
	itemCheckUpd   *systray.MenuItem
	itemAutostart  *systray.MenuItem
	itemDashboard  *systray.MenuItem
	itemRelogin    *systray.MenuItem
	itemLogout     *systray.MenuItem
	itemQuit       *systray.MenuItem

	watcherDone     chan struct{}
	watcherDoneOnce sync.Once

	// Last SetIcon target, to skip no-op SetIcon calls each refresh tick.
	currentIcon []byte
)

func onTrayReady() {
	currentIcon = iconOffline
	systray.SetIcon(currentIcon)
	systray.SetTitle("")
	systray.SetTooltip("StreamTrackr Companion")

	itemStatus = systray.AddMenuItem("Status: starting…", "")
	itemStatus.Disable()
	itemAccount = systray.AddMenuItem("", "")
	itemAccount.Disable()
	itemAccount.Hide()
	itemGame = systray.AddMenuItem("Game: —", "")
	itemGame.Disable()
	itemLastUnlock = systray.AddMenuItem("Last achievement: —", "")
	itemLastUnlock.Disable()

	systray.AddSeparator()

	itemRestart = systray.AddMenuItem("Update ready — Restart now", "Relaunch the companion to apply the staged update")
	itemRestart.Hide()

	itemDashboard = systray.AddMenuItem("Open dashboard", "Open streamtrackr.com/dashboard in the browser")
	itemRelogin = systray.AddMenuItem("Sign in again…", "Re-run the OAuth pairing flow in the browser")
	itemLogout = systray.AddMenuItem("Sign out of StreamTrackr", "Revoke this companion's token and unpair the app")
	itemLogout.Hide()
	itemCheckUpd = systray.AddMenuItem("Check for updates", "Force a check against the release manifest")
	itemAutostart = systray.AddMenuItemCheckbox("Launch at startup", "Auto-start when Windows boots", autostartEnabled())

	systray.AddSeparator()
	itemQuit = systray.AddMenuItem("Quit", "Stop the companion and exit")

	watcherDone = make(chan struct{})
	// Supervised: if the watcher panics or returns while the app is
	// still up, it comes back and the log says so. An unsupervised
	// watcher that stops is indistinguishable from a working one.
	supervise("watcher", watcherDone, func() { runWatcherLoopForTray(watcherDone) })
	startAutoUpdater(6*time.Hour, watcherDone)
	supervise("health monitor", watcherDone, func() { runHealthMonitor(watcherDone) })
	supervise("menu refresh", watcherDone, refreshMenuLoop)
	supervise("menu events", watcherDone, handleMenuEvents)
}

func onTrayExit() {
	stopWatcher()
	logf("tray exit — companion shutting down.")
}

func stopWatcher() {
	watcherDoneOnce.Do(func() {
		if watcherDone != nil {
			close(watcherDone)
		}
	})
}

// refreshMenuLoop redraws the info rows at 1 Hz. Must exit on
// watcherDone to avoid racing systray.Quit's teardown.
func refreshMenuLoop() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-watcherDone:
			return
		case <-t.C:
			updateMenuFromState()
		}
	}
}

func updateMenuFromState() {
	s := state.snapshot()

	if updateReady() {
		itemRestart.Show()
	}

	// The dot follows health(), which is about delivery — whether what
	// we send is being accepted — and not about whether a token exists.
	// The old icon logic answered "am I connected", which is the
	// question that made three different outages look identical.
	level, label := s.health(time.Now())

	var next []byte
	switch {
	case level == healthRed && !s.Authenticated:
		next = iconOffline
	case level == healthRed:
		next = iconError
	case level == healthAmber:
		next = iconWarn
	case s.CurrentAppID == 0:
		next = iconIdle
	default:
		next = iconActive
	}
	if !bytes.Equal(next, currentIcon) {
		systray.SetIcon(next)
		currentIcon = next
	}

	switch level {
	case healthRed:
		itemStatus.SetTitle("Status: ✖ " + truncate(label, 70))
	case healthAmber:
		itemStatus.SetTitle("Status: ⚠ " + truncate(label, 70))
	default:
		itemStatus.SetTitle("Status: " + truncate(label, 70))
	}

	identity := s.UserDisplayName
	if identity == "" {
		identity = s.UserEmail
	}
	if s.Authenticated && identity != "" {
		itemAccount.SetTitle("Logged in as " + truncate(identity, 40))
		itemAccount.Show()
	} else {
		itemAccount.Hide()
	}

	if s.Authenticated {
		itemLogout.Show()
	} else {
		itemLogout.Hide()
	}

	if s.CurrentAppID == 0 {
		itemGame.SetTitle("Game: —")
	} else if s.CurrentGameName != "" {
		itemGame.SetTitle(fmt.Sprintf("Game: %s (appid %d)", s.CurrentGameName, s.CurrentAppID))
	} else {
		itemGame.SetTitle(fmt.Sprintf("Game: appid %d", s.CurrentAppID))
	}

	if s.LastUnlockTitle == "" {
		itemLastUnlock.SetTitle("Last achievement: —")
	} else {
		ago := humanRelative(time.Since(s.LastUnlockAt))
		itemLastUnlock.SetTitle(fmt.Sprintf("Last achievement: %s · %s", truncate(s.LastUnlockTitle, 48), ago))
	}

	if level != healthGreen {
		systray.SetTooltip("StreamTrackr Companion — " + truncate(label, 100))
	} else if s.CurrentAppID != 0 {
		systray.SetTooltip(fmt.Sprintf("StreamTrackr Companion — %s (%d unlocks)", s.CurrentGameName, s.UnlocksPushedThisSession))
	} else {
		systray.SetTooltip("StreamTrackr Companion — waiting for a game")
	}
}

func handleMenuEvents() {
	for {
		select {
		// Shutdown is a legitimate way out of this loop, and it has to
		// be one the supervisor recognises. Without this case, quitting
		// raced: the loop returned before watcherDone was closed, the
		// supervisor read that as a crash, and the restarted loop then
		// read the already-closed ClickedCh channels and re-triggered
		// the whole teardown.
		case <-watcherDone:
			return
		case <-itemRestart.ClickedCh:
			// On success this never returns. On failure it must NOT end
			// the loop: returning here left the whole menu inert —
			// Quit included — with nothing said about it.
			if err := restartSelf(); err != nil {
				logf("restart: %v — companion left running", err)
			}
		case <-itemDashboard.ClickedCh:
			_ = openBrowser(deriveFrontendURL(currentBackend()) + "/dashboard")
		case <-itemRelogin.ClickedCh:
			// Async so the menu stays responsive (login can wait 10 min).
			go func() {
				backend := currentBackend()
				front := deriveFrontendURL(backend)
				if err := runLoginE(backend, front, defaultLabel()); err != nil {
					logf("re-login: %v", err)
					return
				}
				if t, _, err := loadToken(); err == nil && t != "" {
					// applyToken, not setAuthenticated: this line used to
					// update the UI flag only, leaving the watcher on the
					// old (or empty) token. That is the bug that produced
					// a green dot and zero requests, indefinitely.
					applyToken(t)
					logf("re-login: token applied — pushes resume with the new token")
					go refreshIdentity(backend, t)
				}
			}()
		case <-itemLogout.ClickedCh:
			// Async — revoke can hit a 5 s timeout. Local state is torn
			// down even if the server-side revoke fails.
			go func() {
				if err := revokeSelf(currentBackend(), currentToken()); err != nil {
					logf("logout: server-side revoke failed: %v (clearing locally anyway)", err)
				}
				if err := clearToken(); err != nil {
					logf("logout: clearToken failed: %v", err)
				}
				applyToken("")
				state.setIdentity("", "")
			}()
		case <-itemCheckUpd.ClickedCh:
			go runManualUpdateCheck()
		case <-itemAutostart.ClickedCh:
			toggleAutostart(itemAutostart)
		case <-itemQuit.ClickedCh:
			// Close done first: it tells every supervised loop —
			// including this one — that stopping is intended.
			stopWatcher()
			systray.Quit()
			return
		}
	}
}

func deriveFrontendURL(backend string) string {
	if v := envOr("STREAMTRACKR_FRONTEND", ""); v != "" {
		return v
	}
	for _, prefix := range []string{"https://api.", "http://api."} {
		if len(backend) > len(prefix) && backend[:len(prefix)] == prefix {
			scheme := prefix[:len(prefix)-4]
			return scheme + backend[len(prefix):]
		}
	}
	return backend
}

// truncate / humanRelative live in health.go — the CLI and the tests
// need them on every platform.
