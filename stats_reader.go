package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Steam stores per-app achievement state in two BinaryKV files under
// <Steam>\appcache\stats\:
//
//   UserGameStatsSchema_<appid>.bin        achievement schema (apiNames + bit layout)
//   UserGameStats_<steamid3>_<appid>.bin   current per-user values, rewritten on every StoreStats

// Schema "type" field — kept for reference when debugging dumps.
// Selection of achievement stats is driven by presence of "bits", not
// by this value (which is sometimes int, sometimes string, sometimes
// absent depending on the game).
const (
	statTypeInt          = 1
	statTypeFloat        = 2
	statTypeAvgRate      = 3
	statTypeAchievements = 4
)

// achievementSlot pins one achievement to a bit inside one stat. Up to
// 32 slots share a statID since each stat's value is an int32.
type achievementSlot struct {
	APIName string
	StatID  uint32
	Bit     uint32
}

func statsCachePath(steamPath string, steamID3 uint32, appid uint32) string {
	return filepath.Join(steamPath, "appcache", "stats",
		fmt.Sprintf("UserGameStats_%d_%d.bin", steamID3, appid))
}

func schemaCachePath(steamPath string, appid uint32) string {
	return filepath.Join(steamPath, "appcache", "stats",
		fmt.Sprintf("UserGameStatsSchema_%d.bin", appid))
}

// readSchema parses the schema cache and returns the flat list of
// (statID, bit, apiName) slots. Empty slice means "file readable, no
// achievements" — a valid state for some games and not an error.
func readSchema(steamPath string, appid uint32) ([]achievementSlot, error) {
	data, err := os.ReadFile(schemaCachePath(steamPath, appid))
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	root, err := parseBinaryKV(data)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return extractSlots(root), nil
}

func extractSlots(root *kvNode) []achievementSlot {
	stats := root.Child("stats")
	if stats == nil {
		return nil
	}
	var slots []achievementSlot
	for _, stat := range stats.Children {
		if stat.Type != kvNone {
			continue
		}
		var statID uint32
		if _, err := fmt.Sscanf(stat.Name, "%d", &statID); err != nil {
			continue
		}
		// "bits" presence is the load-bearing signal — the "type" field
		// is unreliable across games (int 4, string "ACHIEVEMENTS", or
		// absent for some indies).
		bits := stat.Child("bits")
		if bits == nil {
			continue
		}
		for _, bitNode := range bits.Children {
			if bitNode.Type != kvNone {
				continue
			}
			var bit uint32
			if _, err := fmt.Sscanf(bitNode.Name, "%d", &bit); err != nil {
				continue
			}
			if bit >= 32 {
				continue
			}
			name := bitNode.Child("name").AsString()
			if name == "" {
				continue
			}
			slots = append(slots, achievementSlot{APIName: name, StatID: statID, Bit: bit})
		}
	}
	return slots
}

// readUserStats returns the current int32 of each stat keyed by
// statID. ENOENT yields an empty map (no error) — that's the normal
// state until the user's first StoreStats for this app.
func readUserStats(steamPath string, steamID3 uint32, appid uint32) (map[uint32]int32, error) {
	data, err := os.ReadFile(statsCachePath(steamPath, steamID3, appid))
	if os.IsNotExist(err) {
		return map[uint32]int32{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read user stats: %w", err)
	}
	return parseUserStats(data)
}

// readExistingUserStats is readUserStats for a file we just saw change.
// There a missing or empty file is Steam mid-rewrite, not "no stats yet"
// — and read as all-locked it would push a relock of the entire game.
// A file truncated mid-object is refused by the parser for the same
// reason; the caller confirms a relock against a second read on top of
// that, for a partial write that happens to parse.
func readExistingUserStats(steamPath string, steamID3 uint32, appid uint32) (map[uint32]int32, error) {
	data, err := os.ReadFile(statsCachePath(steamPath, steamID3, appid))
	if err != nil {
		return nil, fmt.Errorf("read user stats: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("read user stats: empty file")
	}
	return parseUserStats(data)
}

func parseUserStats(data []byte) (map[uint32]int32, error) {
	root, err := parseBinaryKV(data)
	if err != nil {
		return nil, fmt.Errorf("parse user stats: %w", err)
	}
	return extractStatValues(root), nil
}

// extractStatValues reads the "data" field off each numeric-keyed child
// of the stats root. The field is "data", NOT "Value" — the Steamworks
// public docs are misleading on this point; Steam's actual on-disk
// cache writes "data". Sibling keys like "crc"/"PendingChanges" parse
// fine but get filtered out because their names aren't numeric.
func extractStatValues(root *kvNode) map[uint32]int32 {
	out := make(map[uint32]int32)
	for _, stat := range root.Children {
		if stat.Type != kvNone {
			continue
		}
		var statID uint32
		if _, err := fmt.Sscanf(stat.Name, "%d", &statID); err != nil {
			continue
		}
		out[statID] = int32(stat.Child("data").AsInt())
	}
	return out
}

// computeUnlocked returns map[apiName]bool covering every slot in the
// schema — locked entries are present with value false so callers can
// take a reliable baseline diff.
func computeUnlocked(slots []achievementSlot, stats map[uint32]int32) map[string]bool {
	out := make(map[string]bool, len(slots))
	for _, s := range slots {
		v := uint32(stats[s.StatID])
		out[s.APIName] = (v & (1 << s.Bit)) != 0
	}
	return out
}

// relockedSince / unlockedSince are the two transitions the companion
// pushes. Sorted, so a log line, a payload and a test all read the same
// way twice in a row — map iteration order would make all three jitter.
func relockedSince(prev, next map[string]bool) []string {
	var out []string
	for apiName, isUnlocked := range next {
		if !isUnlocked && prev[apiName] {
			out = append(out, apiName)
		}
	}
	sort.Strings(out)
	return out
}

func unlockedSince(prev, next map[string]bool) []string {
	var out []string
	for apiName, isUnlocked := range next {
		if isUnlocked && !prev[apiName] {
			out = append(out, apiName)
		}
	}
	sort.Strings(out)
	return out
}

// sameUnlocks reports whether two reads of the stats file agree.
func sameUnlocks(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for apiName, isUnlocked := range a {
		if b[apiName] != isUnlocked {
			return false
		}
	}
	return true
}

// joinNames renders achievement names for a log line, capped so a whole
// game re-locked at once stays one readable line.
func joinNames(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s … +%d more", strings.Join(names[:max], ", "), len(names)-max)
}
