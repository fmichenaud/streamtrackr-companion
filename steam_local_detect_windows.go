//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// Local Steam state detection — primary path, faster than the backend
// /current-game endpoint and free of any dependency on the user having
// a Steam tracker on their StreamTrackr account.

// detectLocalGame returns the currently-running appid + display name.
// (0, "", nil) means no game; (appid, "", nil) means we got the appid
// but couldn't read the name from the appmanifest; non-nil err means
// the registry itself was unreadable.
func detectLocalGame() (appid uint32, name string, err error) {
	appid, err = readRunningAppID()
	if err != nil {
		return 0, "", err
	}
	if appid == 0 {
		return 0, "", nil
	}

	// Best-effort name lookup — failures fall back to "appid N" in the UI.
	steamPath, perr := readSteamPath()
	if perr != nil {
		return appid, "", nil
	}
	name, _ = readAppManifestName(steamPath, appid)
	return appid, name, nil
}

// readRunningAppID reads the Steam-native zero-latency signal for
// "user is in a game right now".
func readRunningAppID() (uint32, error) {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Valve\Steam`,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return 0, fmt.Errorf("open Steam registry key: %w", err)
	}
	defer key.Close()

	v, _, err := key.GetIntegerValue("RunningAppID")
	if err != nil {
		// Value-missing happens on fresh installs before any game launch.
		if err == registry.ErrNotExist {
			return 0, nil
		}
		return 0, fmt.Errorf("read RunningAppID: %w", err)
	}
	return uint32(v), nil
}

// readSteamPath returns the Steam install directory (follows custom
// install locations).
func readSteamPath() (string, error) {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Valve\Steam`,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return "", fmt.Errorf("open Steam registry key: %w", err)
	}
	defer key.Close()

	v, _, err := key.GetStringValue("SteamPath")
	if err != nil {
		return "", fmt.Errorf("read SteamPath: %w", err)
	}
	return filepath.FromSlash(v), nil // Steam stores forward slashes here
}

// readAppManifestName scans every Steam library for the appmanifest's
// "name" field. Multi-library installs are common (small SSD + large
// HDD) so a single-location lookup would miss most games.
func readAppManifestName(steamPath string, appid uint32) (string, error) {
	libs, err := readLibraryFolders(steamPath)
	if err != nil {
		libs = []string{steamPath} // single-library fallback
	}

	manifestName := fmt.Sprintf("appmanifest_%d.acf", appid)
	for _, lib := range libs {
		path := filepath.Join(lib, "steamapps", manifestName)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if name := extractACFName(string(data)); name != "" {
			return name, nil
		}
	}
	return "", fmt.Errorf("appmanifest_%d.acf not found in any library", appid)
}

// readLibraryFolders parses every "path" entry in libraryfolders.vdf.
func readLibraryFolders(steamPath string) ([]string, error) {
	path := filepath.Join(steamPath, "steamapps", "libraryfolders.vdf")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return extractVDFPaths(string(data)), nil
}

// extractACFName + extractVDFPaths live in steam_local_detect.go so
// they're unit-testable from any OS.
