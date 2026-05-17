//go:build !windows

package main

// Non-Windows stubs for the dialog helpers — there's no tray on these
// platforms, but the tray-mode code references them unconditionally so
// the package needs symbols of the right shape for `go build` to pass.
//
// Behaviour is deliberately minimal: info/error dialogs log and move on,
// yes/no defaults to "no" so the auto-update path doesn't proceed without
// a UI to confirm.
func showInfoDialog(title, text string)  { logf("dialog (info) %s: %s", title, text) }
func showErrorDialog(title, text string) { logf("dialog (err)  %s: %s", title, text) }
func showYesNoDialog(title, text string) bool {
	logf("dialog (Y/N) %s: %s — defaulting to No on non-Windows", title, text)
	return false
}
