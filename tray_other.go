//go:build !windows

package main

// Stub for non-Windows dev builds. The tray icon lives in tray_windows.go
// because systray pulls in OS-specific cgo, and the appcache + registry
// detection paths are Windows-specific anyway. On macOS / Linux we just
// fail gracefully — there's no realistic deployment of this companion
// outside Windows.

import (
	"fmt"
	"os"
)

func runTray() {
	fmt.Fprintln(os.Stderr, "Tray mode is Windows-only. Use --cli on this platform.")
	os.Exit(1)
}
