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
	go runWatcherLoopForTray(watcherDone)
	startAutoUpdater(6*time.Hour, watcherDone)
	go refreshMenuLoop()
	go handleMenuEvents()
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

	// Icon priority: error trumps "no game" — push failures are
	// more actionable than waiting state.
	var next []byte
	switch {
	case !s.Authenticated:
		next = iconOffline
	case s.LastPushError != "":
		next = iconError
	case s.CurrentAppID == 0:
		next = iconIdle
	default:
		next = iconActive
	}
	if !bytes.Equal(next, currentIcon) {
		systray.SetIcon(next)
		currentIcon = next
	}

	switch {
	case !s.Authenticated:
		itemStatus.SetTitle("Status: not signed in (click 'Sign in again')")
	case s.CurrentAppID == 0:
		itemStatus.SetTitle("Status: waiting for a Steam game")
	case s.LastPushError != "":
		itemStatus.SetTitle("Status: ⚠ " + truncate(s.LastPushError, 60))
	default:
		itemStatus.SetTitle(fmt.Sprintf("Status: active · %d unlock(s) this session", s.UnlocksPushedThisSession))
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

	if s.LastPushError != "" {
		systray.SetTooltip("StreamTrackr Companion — error: " + truncate(s.LastPushError, 80))
	} else if s.CurrentAppID != 0 {
		systray.SetTooltip(fmt.Sprintf("StreamTrackr Companion — %s (%d unlocks)", s.CurrentGameName, s.UnlocksPushedThisSession))
	} else {
		systray.SetTooltip("StreamTrackr Companion — waiting for a game")
	}
}

func handleMenuEvents() {
	for {
		select {
		case <-itemRestart.ClickedCh:
			restartSelf()
			return
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
					state.setAuthenticated(true)
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
				state.setAuthenticated(false)
				state.setIdentity("", "")
				setToken("")
			}()
		case <-itemCheckUpd.ClickedCh:
			go runManualUpdateCheck()
		case <-itemAutostart.ClickedCh:
			toggleAutostart(itemAutostart)
		case <-itemQuit.ClickedCh:
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func humanRelative(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dmin ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02d ago", int(d.Hours()), int(d.Minutes())%60)
	}
}
