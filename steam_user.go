package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// Steam stores per-user state under <Steam>\userdata\<steamid3>\ and
// the stats cache at <Steam>\appcache\stats\UserGameStats_<steamid3>_<appid>.bin.
// We discover the active steamID3 from <Steam>\config\loginusers.vdf
// (the user with MostRecent = 1).

const steamIDBase uint64 = 0x0110000100000000 // SteamID64 → AccountID base

var loginUsersBlockStartRe = regexp.MustCompile(`"(\d{17})"\s*\{`)

func readCurrentSteamID3(steamPath string) (uint32, error) {
	path := filepath.Join(steamPath, "config", "loginusers.vdf")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return parseCurrentSteamID3(string(data))
}

func parseCurrentSteamID3(content string) (uint32, error) {
	starts := loginUsersBlockStartRe.FindAllStringSubmatchIndex(content, -1)
	if len(starts) == 0 {
		return 0, fmt.Errorf("loginusers.vdf: no user blocks found")
	}
	for i, m := range starts {
		blockStart := m[1]
		blockEnd := len(content)
		if i+1 < len(starts) {
			blockEnd = starts[i+1][0]
		}
		if !isMostRecentTrue(content[blockStart:blockEnd]) {
			continue
		}
		steamID64Str := content[m[2]:m[3]]
		steamID64, err := strconv.ParseUint(steamID64Str, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse steamid64 %q: %w", steamID64Str, err)
		}
		if steamID64 < steamIDBase {
			return 0, fmt.Errorf("steamid64 %d below known base — corrupt file?", steamID64)
		}
		return uint32(steamID64 & 0xFFFFFFFF), nil
	}
	return 0, fmt.Errorf("loginusers.vdf: no user with MostRecent=1")
}

var mostRecentRe = regexp.MustCompile(`"MostRecent"\s+"(\d+)"`)

func isMostRecentTrue(block string) bool {
	m := mostRecentRe.FindStringSubmatch(block)
	return m != nil && m[1] == "1"
}

func steamID64FromAccountID(accountID uint32) uint64 {
	return uint64(accountID) | steamIDBase
}
