//go:build windows

package main

import (
	"fmt"
	"time"
)

// runManualUpdateCheck drives the "Check for updates" menu action:
// fetch manifest → confirm download → apply → confirm restart. Resets
// the menu label on exit regardless of which branch was taken.
func runManualUpdateCheck() {
	itemCheckUpd.SetTitle("Searching…")
	itemCheckUpd.Disable()
	defer func() {
		// 1 s delay so the label change is visible even on instant
		// fetches.
		time.AfterFunc(1*time.Second, func() {
			itemCheckUpd.SetTitle("Check for updates")
			itemCheckUpd.Enable()
		})
	}()

	m, err := fetchManifest()
	if err != nil {
		logf("manual update check: %v", err)
		showErrorDialog(
			"StreamTrackr Companion",
			"Update check failed.\n\n"+
				"Couldn't reach the update server. Check your internet\n"+
				"connection and try again later.\n\n"+
				"Details: "+err.Error(),
		)
		return
	}
	if m == nil || !isNewer(m.Version, version) {
		showInfoDialog(
			"StreamTrackr Companion",
			fmt.Sprintf(
				"You're already running the latest version (v%s).\n\n"+
					"The companion will automatically check for new updates\n"+
					"in the background.",
				version,
			),
		)
		return
	}

	// Ask before downloading — the user's chance to defer if they're
	// streaming and don't want a download competing for bandwidth.
	if !showYesNoDialog(
		"Update available",
		fmt.Sprintf(
			"A new version of StreamTrackr Companion is available.\n\n"+
				"   Installed : v%s\n"+
				"   Available : v%s\n\n"+
				"Download the update now?",
			version, m.Version,
		),
	) {
		logf("manual update check: user declined download of v%s", m.Version)
		return
	}

	itemCheckUpd.SetTitle("Downloading…")

	if err := applyManifest(m); err != nil {
		logf("manual update check: apply failed: %v", err)
		showErrorDialog(
			"StreamTrackr Companion",
			"The update could not be installed.\n\n"+
				"Details: "+err.Error()+"\n\n"+
				"Please try again later.",
		)
		return
	}

	// On decline, the tray's "Update ready — Restart now" row stays
	// visible (driven by updateReady() in updateMenuFromState).
	if showYesNoDialog(
		"Update installed",
		fmt.Sprintf(
			"Version v%s has been downloaded and installed.\n\n"+
				"Restart StreamTrackr Companion now to apply it?",
			m.Version,
		),
	) {
		restartSelf()
	}
}
