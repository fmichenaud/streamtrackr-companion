package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Steam stores per-user state under <Steam>\userdata\<steamid3>\ and
// the stats cache at <Steam>\appcache\stats\UserGameStats_<steamid3>_<appid>.bin.
//
// Answering "which account is signed in right now" has three sources,
// tried in order of trustworthiness:
//
//  1. HKCU\Software\Valve\Steam\ActiveProcess\ActiveUser — written live
//     by the running client, exact, non-zero whenever Steam is up. Since
//     we only resolve an account while a game is running, Steam is up by
//     construction and this is the path that normally fires.
//  2. config\loginusers.vdf — MostRecent=1, else the newest Timestamp,
//     else the sole account listed.
//  3. userdata\<steamid3>\ — the sole directory, else the most recently
//     modified one.
//
// The chain exists because a July 2026 Steam client update stopped
// writing MostRecent on some installs. Depending on that single key left
// the watcher with no account at all: no stats file to watch, no unlocks
// detected, silently, for the whole session.

const steamIDBase uint64 = 0x0110000100000000 // SteamID64 → AccountID base

// A SteamID64 is 17 digits today, but match a wider range and let the
// steamIDBase check do the validating — so a future digit rollover
// doesn't strand us the same way MostRecent did.
var loginUsersBlockStartRe = regexp.MustCompile(`"(\d{15,20})"\s*\{`)

// Valve's KeyValues format is case-insensitive and the client has
// written both spellings over the years — match either.
var (
	mostRecentRe = regexp.MustCompile(`(?i)"MostRecent"\s*"(\d+)"`)
	timestampRe  = regexp.MustCompile(`(?i)"Timestamp"\s*"(\d+)"`)
)

// resolveSteamID3 returns the active account ID plus a short label
// naming the source that produced it. The label is logged so a support
// log says which link of the chain fired.
func resolveSteamID3(steamPath string) (uint32, string, error) {
	var attempts []string

	id, err := readActiveUserID3()
	switch {
	case err != nil:
		attempts = append(attempts, "registry ActiveUser: "+err.Error())
	case id != 0:
		return id, "registry ActiveUser", nil
	default:
		attempts = append(attempts, "registry ActiveUser: 0 (Steam signed out)")
	}

	if id, source, err := readSteamID3FromLoginUsers(steamPath); err == nil {
		return id, source, nil
	} else {
		attempts = append(attempts, "loginusers.vdf: "+err.Error())
	}

	if id, source, err := readSteamID3FromUserdata(steamPath); err == nil {
		return id, source, nil
	} else {
		attempts = append(attempts, "userdata: "+err.Error())
	}

	return 0, "", fmt.Errorf("no signed-in Steam account found — %s", strings.Join(attempts, "; "))
}

// ─────────────────────────── loginusers.vdf ────────────────────────────

func readSteamID3FromLoginUsers(steamPath string) (uint32, string, error) {
	path := filepath.Join(steamPath, "config", "loginusers.vdf")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", fmt.Errorf("read %s: %w", path, err)
	}
	return parseLoginUsers(string(data))
}

type loginUserEntry struct {
	id3        uint32
	mostRecent bool
	timestamp  uint64
}

// parseLoginUsers picks the signed-in account from loginusers.vdf.
// MostRecent=1 wins when present; otherwise the newest Timestamp; a
// lone account is taken as-is. Several accounts with no usable
// discriminator is an error rather than a guess — picking wrong means
// watching a stats file that never changes, which looks exactly like
// "the companion is broken" to the user.
func parseLoginUsers(content string) (uint32, string, error) {
	starts := loginUsersBlockStartRe.FindAllStringSubmatchIndex(content, -1)
	if len(starts) == 0 {
		return 0, "", fmt.Errorf("no user blocks found")
	}

	var entries []loginUserEntry
	var rejected []string
	for i, m := range starts {
		blockEnd := len(content)
		if i+1 < len(starts) {
			blockEnd = starts[i+1][0]
		}
		block := content[m[1]:blockEnd]

		raw := content[m[2]:m[3]]
		steamID64, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || steamID64 < steamIDBase {
			// Below the base is malformed — Valve has never minted
			// account IDs there.
			rejected = append(rejected, raw)
			continue
		}
		entries = append(entries, loginUserEntry{
			id3:        uint32(steamID64 & 0xFFFFFFFF),
			mostRecent: firstUint(mostRecentRe, block) == 1,
			timestamp:  firstUint(timestampRe, block),
		})
	}

	if len(entries) == 0 {
		return 0, "", fmt.Errorf("no valid steamID64 in file (rejected: %s)", strings.Join(rejected, ", "))
	}

	for _, e := range entries {
		if e.mostRecent {
			return e.id3, "loginusers.vdf MostRecent", nil
		}
	}

	if len(entries) == 1 {
		return entries[0].id3, "loginusers.vdf sole account", nil
	}

	best := entries[0]
	for _, e := range entries[1:] {
		if e.timestamp > best.timestamp {
			best = e
		}
	}
	if best.timestamp == 0 {
		return 0, "", fmt.Errorf("%d accounts, none with MostRecent=1 or a Timestamp", len(entries))
	}
	return best.id3, "loginusers.vdf newest Timestamp", nil
}

func firstUint(re *regexp.Regexp, block string) uint64 {
	m := re.FindStringSubmatch(block)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ───────────────────────────── userdata\ ───────────────────────────────

// readSteamID3FromUserdata is the last resort: every account that has
// ever signed in on this machine owns a <Steam>\userdata\<steamid3>
// directory. Steam rewrites files under the active account's directory
// continuously, so the most recently modified one is the current player
// on a multi-account machine.
func readSteamID3FromUserdata(steamPath string) (uint32, string, error) {
	dir := filepath.Join(steamPath, "userdata")
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0, "", fmt.Errorf("read %s: %w", dir, err)
	}

	var (
		best     uint32
		bestMod  int64
		found    int
		firstID3 uint32
	)
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		// Steam parks an "0" and sometimes "ac" directory here — neither
		// is an account.
		id, err := strconv.ParseUint(item.Name(), 10, 32)
		if err != nil || id == 0 {
			continue
		}
		found++
		if found == 1 {
			firstID3 = uint32(id)
		}
		info, err := item.Info()
		if err != nil {
			continue
		}
		if mod := info.ModTime().UnixNano(); mod > bestMod {
			bestMod, best = mod, uint32(id)
		}
	}

	switch {
	case found == 0:
		return 0, "", fmt.Errorf("no account directory in %s", dir)
	case found == 1:
		return firstID3, "userdata sole account", nil
	case best != 0:
		return best, "userdata most recently modified", nil
	default:
		return 0, "", fmt.Errorf("%d account directories, none readable", found)
	}
}

func steamID64FromAccountID(accountID uint32) uint64 {
	return uint64(accountID) | steamIDBase
}
