//go:build !windows

package main

// On non-Windows builds (dev only) there's no native message-box flow.
// Always return true so the auto-login path proceeds.
func showWelcomeDialog() bool { return true }
