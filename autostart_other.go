//go:build !windows

package main

// Stub: auto-start lives in the Windows registry. macOS / Linux dev
// builds don't have a usable target here.

func autostartEnabled() bool                  { return false }
func setAutostart(_ bool) (bool, error)       { return false, nil }
