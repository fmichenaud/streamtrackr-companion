//go:build windows

package main

import (
	"os"
	"syscall"
)

// ensureConsole reconnects stdout/stderr when the -H=windowsgui binary
// is invoked from a terminal. Tries AttachConsole(parent) first, falls
// back to AllocConsole, then rebinds os.Stdout/Stderr to CONOUT$.
func ensureConsole() {
	const (
		attachParent = ^uint32(0) // (DWORD)-1 = ATTACH_PARENT_PROCESS
	)
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procAttach := kernel32.NewProc("AttachConsole")
	procAlloc := kernel32.NewProc("AllocConsole")

	r1, _, _ := procAttach.Call(uintptr(attachParent))
	if r1 == 0 {
		_, _, _ = procAlloc.Call()
	}

	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = f
	}
}
