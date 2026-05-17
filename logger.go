package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger for tray mode (no console). Rotates to .1 at maxLogBytes,
// keeps a single backup.
const maxLogBytes = 5 * 1024 * 1024 // 5 MiB

var (
	logMu       sync.Mutex
	logFile     *os.File
	logPath     string
	logToStderr bool // true in CLI mode so the user still sees output
)

func logPathDefault() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "StreamTrackr", "companion.log"), nil
}

// initLogger opens the rotating log file. tee=true also writes to
// stderr (CLI mode). Idempotent.
func initLogger(tee bool) error {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		return nil
	}

	path, err := logPathDefault()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", path, err)
	}

	logFile = f
	logPath = path
	logToStderr = tee

	if tee {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	} else {
		log.SetOutput(logFile)
	}

	fmt.Fprintf(logFile, "\n=== session start %s pid=%d ===\n",
		time.Now().Format(time.RFC3339), os.Getpid())
	return nil
}

// rotateIfNeeded is called inline from logf() so no goroutine is needed.
func rotateIfNeeded() {
	if logFile == nil {
		return
	}
	fi, err := logFile.Stat()
	if err != nil || fi.Size() < maxLogBytes {
		return
	}
	logFile.Close()
	_ = os.Remove(logPath + ".1")
	_ = os.Rename(logPath, logPath+".1")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		logFile = nil
		if logToStderr {
			log.SetOutput(os.Stderr)
		}
		return
	}
	logFile = f
	if logToStderr {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
	} else {
		log.SetOutput(logFile)
	}
}

// logf is the canonical write path: thread-safe, prefixed with HH:MM:SS,
// auto-rotates.
func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	line := fmt.Sprintf(format, args...)
	if !endsWithNewline(line) {
		line += "\n"
	}
	prefixed := time.Now().Format("15:04:05.000 ") + line
	if logFile != nil {
		rotateIfNeeded()
		logFile.WriteString(prefixed)
	}
	if logToStderr {
		fmt.Fprint(os.Stderr, prefixed)
	}
}

func endsWithNewline(s string) bool {
	return len(s) > 0 && s[len(s)-1] == '\n'
}
