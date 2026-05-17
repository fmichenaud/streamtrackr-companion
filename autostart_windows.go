//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows/registry"
)

// HKCU (not HKLM): no admin needed, scoped to the installing user.
const (
	autostartRunKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartValueKey = "StreamTrackrCompanion"
)

// autostartEnabled returns true only when the Run-key entry points at
// the current exe — stale entries from old install paths count as off
// so the user can re-toggle cleanly.
func autostartEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	current, _, err := key.GetStringValue(autostartValueKey)
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	// Stored quoted (paths with spaces), compared unquoted.
	if len(current) >= 2 && current[0] == '"' && current[len(current)-1] == '"' {
		current = current[1 : len(current)-1]
	}
	return current == exe
}

// setAutostart writes or removes the Run-key entry. Returns the new
// state so the tray checkbox doesn't desync on registry write failure.
func setAutostart(enable bool) (bool, error) {
	if enable {
		exe, err := os.Executable()
		if err != nil {
			return false, fmt.Errorf("Executable(): %w", err)
		}
		key, _, err := registry.CreateKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
		if err != nil {
			return false, fmt.Errorf("CreateKey: %w", err)
		}
		defer key.Close()
		// Quoted: Windows shells split on space otherwise.
		if err := key.SetStringValue(autostartValueKey, `"`+exe+`"`); err != nil {
			return false, fmt.Errorf("SetStringValue: %w", err)
		}
		return true, nil
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return false, nil
	}
	defer key.Close()
	if err := key.DeleteValue(autostartValueKey); err != nil && err != registry.ErrNotExist {
		return false, fmt.Errorf("DeleteValue: %w", err)
	}
	return false, nil
}

// toggleAutostart flips based on current registry state, not the
// menu's own Checked() — keeps us correct if an external tool edited
// the registry behind our back.
func toggleAutostart(item *systray.MenuItem) {
	target := !autostartEnabled()
	now, err := setAutostart(target)
	if err != nil {
		logf("autostart: %v", err)
		return
	}
	if now {
		item.Check()
	} else {
		item.Uncheck()
	}
}
