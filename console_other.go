//go:build !windows

package main

// On macOS / Linux there's no detached-console problem: a CLI binary
// naturally has stdout/stderr wired to the launching terminal. Stub.
func ensureConsole() {}
