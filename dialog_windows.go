//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Win32 MessageBox flags. welcome_windows.go declares its own subset.
const (
	mbYesNo            = 0x00000004
	mbIconWarning      = 0x00000030
	mbIconQuestion     = 0x00000020
	mbIconError        = 0x00000010
	mbTopmost          = 0x00040000
	mbSetForeground    = 0x00010000
	mbTaskmodal        = 0x00002000
)

const (
	idYes = 6
	idNo  = 7
)

// messageBox forces topmost+foreground on every popup — without this
// flags combo, tray-spawned dialogs sometimes hide behind active windows.
func messageBox(title, text string, flags uintptr) int {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	textPtr, _ := syscall.UTF16PtrFromString(text)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	ret, _, _ := proc.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		flags|mbTopmost|mbSetForeground|mbTaskmodal|mbDefaultDesktopOnly,
	)
	return int(ret)
}

func showInfoDialog(title, text string) {
	messageBox(title, text, mbOK|mbIconInformation)
}

func showErrorDialog(title, text string) {
	messageBox(title, text, mbOK|mbIconError)
}

func showYesNoDialog(title, text string) bool {
	return messageBox(title, text, mbYesNo|mbIconQuestion) == idYes
}
