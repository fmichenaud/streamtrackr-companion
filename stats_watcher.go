package main

import (
	"os"
	"time"
)

// watchStatsFile blocks until done closes, calling onChange whenever
// the file's mtime advances. ENOENT is silent — the file appears on
// the user's first StoreStats and we'll catch the mtime transition
// then. onChange runs on this goroutine; keep it fast.
func watchStatsFile(path string, pollInterval time.Duration, done <-chan struct{}, onChange func()) {
	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			if fi.ModTime().Equal(lastMod) {
				continue
			}
			lastMod = fi.ModTime()
			onChange()
		}
	}
}
