//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Non-Windows stubs so go build resolves every symbol on dev machines.

func detectLocalGame() (uint32, string, error) {
	return 0, "", fmt.Errorf("local Steam detection is Windows-only")
}

// readSteamPath supports --dump-stats against a Steam dump copied from
// a Windows machine. Honours $STEAMPATH; falls back to the Linux path.
func readSteamPath() (string, error) {
	if v := os.Getenv("STEAMPATH"); v != "" {
		return v, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		linuxDefault := filepath.Join(home, ".steam", "steam")
		if _, err := os.Stat(linuxDefault); err == nil {
			return linuxDefault, nil
		}
	}
	return "", fmt.Errorf("no Steam install detected — set $STEAMPATH to override")
}
