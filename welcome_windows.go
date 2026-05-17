//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Win32 MessageBox return codes we care about.
const (
	mbOK     = 0x00000000
	mbOKCancel = 0x00000001
	mbIconInformation = 0x00000040
	mbDefaultDesktopOnly = 0x00020000

	idOK     = 1
	idCancel = 2
)

// showWelcomeDialog pops the first-run native MessageBox before pairing.
// Returns true on OK, false on Cancel.
func showWelcomeDialog() bool {
	const text = "Welcome to StreamTrackr Companion! 🎮\n\n" +
		"This app detects your Steam achievements in real time and relays\n" +
		"them instantly to your StreamTrackr account — OBS overlay, Twitch\n" +
		"chat, Discord, stats.\n\n" +
		"How it works:\n" +
		"  1.  Authorize the connection to your StreamTrackr account\n" +
		"      (your browser will open in a moment).\n" +
		"  2.  Launch your Steam games normally.\n" +
		"  3.  Your achievements appear within a second on your overlay.\n\n" +
		"The companion runs quietly in the Windows tray. Right-click the\n" +
		"icon to access options.\n\n" +
		"Click OK to continue and open the authorization page."
	const caption = "StreamTrackr Companion — First launch"

	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")

	textPtr, _ := syscall.UTF16PtrFromString(text)
	captionPtr, _ := syscall.UTF16PtrFromString(caption)

	ret, _, _ := proc.Call(
		0, // tray has no HWND to parent
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(captionPtr)),
		uintptr(mbOKCancel|mbIconInformation|mbDefaultDesktopOnly),
	)
	return ret == idOK
}
