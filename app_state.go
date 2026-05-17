package main

import (
	"sync"
	"time"
)

// appState is the source of truth shared between the watcher goroutine
// (writer) and the tray UI (reader). All access is mutex-guarded.
type appState struct {
	mu sync.RWMutex

	Authenticated bool
	Mode          string // "auto" | "manual" | "idle"

	CurrentAppID    uint32
	CurrentGameName string

	BaselineUnlocked         uint32
	BaselineTotal            uint32
	UnlocksPushedThisSession uint32

	LastUnlockTitle string
	LastUnlockAt    time.Time

	LastPushOK    bool
	LastPushError string

	UserEmail       string
	UserDisplayName string
}

var state = &appState{Mode: "idle"}

func (s *appState) snapshot() appState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return appState{
		Authenticated:            s.Authenticated,
		Mode:                     s.Mode,
		CurrentAppID:             s.CurrentAppID,
		CurrentGameName:          s.CurrentGameName,
		BaselineUnlocked:         s.BaselineUnlocked,
		BaselineTotal:            s.BaselineTotal,
		UnlocksPushedThisSession: s.UnlocksPushedThisSession,
		LastUnlockTitle:          s.LastUnlockTitle,
		LastUnlockAt:             s.LastUnlockAt,
		LastPushOK:               s.LastPushOK,
		LastPushError:            s.LastPushError,
		UserEmail:                s.UserEmail,
		UserDisplayName:          s.UserDisplayName,
	}
}

func (s *appState) setIdentity(email, displayName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.UserEmail = email
	s.UserDisplayName = displayName
}

// setGame resets per-session counters; pass appid=0 for "no game".
func (s *appState) setGame(appid uint32, name string, unlocked, total uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CurrentAppID = appid
	s.CurrentGameName = name
	s.BaselineUnlocked = unlocked
	s.BaselineTotal = total
	s.UnlocksPushedThisSession = 0
}

func (s *appState) recordUnlock(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastUnlockTitle = title
	s.LastUnlockAt = time.Now()
	s.UnlocksPushedThisSession++
}

func (s *appState) recordPush(ok bool, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastPushOK = ok
	if ok {
		s.LastPushError = ""
	} else {
		s.LastPushError = errMsg
	}
}

func (s *appState) setMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Mode = mode
}

func (s *appState) setAuthenticated(authed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Authenticated = authed
}
