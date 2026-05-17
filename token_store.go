package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// tokenFile is the on-disk shape stored in $config/StreamTrackr/token.json.
// Backend URL travels with the token so we don't need a separate flag.
type tokenFile struct {
	Token   string `json:"token"`
	Backend string `json:"backend"`
}

// tokenStorePath resolves to %AppData%\StreamTrackr\token.json on Windows
// (and the equivalent UserConfigDir on macOS/Linux for dev builds).
func tokenStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("UserConfigDir: %w", err)
	}
	return filepath.Join(dir, "StreamTrackr", "token.json"), nil
}

// loadToken returns empty strings (no error) when no file exists yet —
// the expected first-run state. Malformed file is an error.
func loadToken() (token string, backend string, err error) {
	path, err := tokenStorePath()
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	var f tokenFile
	if err := json.Unmarshal(data, &f); err != nil {
		return "", "", fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Token, f.Backend, nil
}

// saveToken writes atomically (tmp + fsync + rename) at mode 0600.
func saveToken(token, backend string) error {
	path, err := tokenStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	data, err := json.MarshalIndent(tokenFile{Token: token, Backend: backend}, "", "  ")
	if err != nil {
		return err
	}

	// fsync before rename so a power loss can't leave a zero-byte file.
	// PID in the tmp name guards against concurrent saveToken callers
	// stomping each other's half-written file.
	tmp := fmt.Sprintf("%s.tmp%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// clearToken removes the persisted token.
func clearToken() error {
	path, err := tokenStorePath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
