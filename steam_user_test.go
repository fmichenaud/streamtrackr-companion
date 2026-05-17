package main

import "testing"

// Real-shape loginusers.vdf — two accounts, the second is MostRecent.
// Sourced from a fresh Steam install on a multi-account machine; the
// formatting (tab indentation, blank lines between blocks) matches
// what Steam writes verbatim.
const sampleLoginUsersTwoAccounts = `"users"
{
	"76561198012345678"
	{
		"AccountName"		"alice"
		"PersonaName"		"Alice"
		"RememberPassword"		"1"
		"WantsOfflineMode"		"0"
		"SkipOfflineModeWarning"		"0"
		"AllowAutoLogin"		"1"
		"MostRecent"		"0"
		"Timestamp"		"1700000000"
	}
	"76561198098765432"
	{
		"AccountName"		"bob"
		"PersonaName"		"Bob"
		"RememberPassword"		"1"
		"MostRecent"		"1"
		"Timestamp"		"1710000000"
	}
}
`

func TestParseCurrentSteamID3_PicksMostRecent(t *testing.T) {
	got, err := parseCurrentSteamID3(sampleLoginUsersTwoAccounts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 76561198098765432 - 0x0110000100000000 = 138499704 (low 32 bits).
	const want uint32 = 138499704
	if got != want {
		t.Errorf("steamID3: got %d, want %d", got, want)
	}
	// Round-trip check — reconstructing the 64-bit form gets the input back.
	if back := steamID64FromAccountID(got); back != 76561198098765432 {
		t.Errorf("round-trip: got %d, want 76561198098765432", back)
	}
}

func TestParseCurrentSteamID3_SingleUser(t *testing.T) {
	const single = `"users"
{
	"76561197960265729"
	{
		"AccountName"		"solo"
		"MostRecent"		"1"
	}
}
`
	got, err := parseCurrentSteamID3(single)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 76561197960265729 - base = 1 (the very first possible account).
	if got != 1 {
		t.Errorf("steamID3: got %d, want 1", got)
	}
}

func TestParseCurrentSteamID3_NoMostRecent(t *testing.T) {
	// Edge case: a user who never selected "Remember password" so no
	// account has MostRecent=1. Must error rather than silently pick
	// the first one — the caller falls back to backend detection.
	const noMostRecent = `"users"
{
	"76561198012345678"
	{
		"AccountName"		"alice"
		"MostRecent"		"0"
	}
}
`
	if _, err := parseCurrentSteamID3(noMostRecent); err == nil {
		t.Error("expected error when no MostRecent=1, got nil")
	}
}

func TestParseCurrentSteamID3_EmptyFile(t *testing.T) {
	if _, err := parseCurrentSteamID3(""); err == nil {
		t.Error("expected error on empty file, got nil")
	}
	if _, err := parseCurrentSteamID3(`"users" {}`); err == nil {
		t.Error("expected error on empty users block, got nil")
	}
}

func TestParseCurrentSteamID3_RejectsLowSteamID(t *testing.T) {
	// Below the base ID is malformed — Valve has never minted account
	// IDs below 0x0110000100000000.
	const bogus = `"users"
{
	"76561197960265727"
	{
		"MostRecent"		"1"
	}
}
`
	if _, err := parseCurrentSteamID3(bogus); err == nil {
		t.Error("expected error on sub-base steamID, got nil")
	}
}
