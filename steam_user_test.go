package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestParseLoginUsers_PicksMostRecent(t *testing.T) {
	got, source, err := parseLoginUsers(sampleLoginUsersTwoAccounts)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 76561198098765432 - 0x0110000100000000 = 138499704 (low 32 bits).
	const want uint32 = 138499704
	if got != want {
		t.Errorf("steamID3: got %d, want %d", got, want)
	}
	if source != "loginusers.vdf MostRecent" {
		t.Errorf("source: got %q", source)
	}
	// Round-trip check — reconstructing the 64-bit form gets the input back.
	if back := steamID64FromAccountID(got); back != 76561198098765432 {
		t.Errorf("round-trip: got %d, want 76561198098765432", back)
	}
}

func TestParseLoginUsers_SingleUser(t *testing.T) {
	const single = `"users"
{
	"76561197960265729"
	{
		"AccountName"		"solo"
		"MostRecent"		"1"
	}
}
`
	got, _, err := parseLoginUsers(single)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 76561197960265729 - base = 1 (the very first possible account).
	if got != 1 {
		t.Errorf("steamID3: got %d, want 1", got)
	}
}

// The July 2026 Steam client update that broke unlock detection: user
// blocks arrive with no MostRecent key at all. Newest Timestamp is the
// stand-in for "who logged in last".
func TestParseLoginUsers_NoMostRecentKey_UsesNewestTimestamp(t *testing.T) {
	const noMostRecentKey = `"users"
{
	"76561198012345678"
	{
		"AccountName"		"alice"
		"Timestamp"		"1700000000"
	}
	"76561198098765432"
	{
		"AccountName"		"bob"
		"Timestamp"		"1710000000"
	}
}
`
	got, source, err := parseLoginUsers(noMostRecentKey)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const want uint32 = 138499704 // bob, the newer Timestamp
	if got != want {
		t.Errorf("steamID3: got %d, want %d", got, want)
	}
	if source != "loginusers.vdf newest Timestamp" {
		t.Errorf("source: got %q", source)
	}
}

// Valve's KeyValues parser is case-insensitive and the client has
// shipped both spellings; ours must not care either.
func TestParseLoginUsers_LowercaseKeys(t *testing.T) {
	const lowercase = `"users"
{
	"76561198012345678"
	{
		"accountname"		"alice"
		"mostrecent"		"0"
		"timestamp"		"1700000000"
	}
	"76561198098765432"
	{
		"accountname"		"bob"
		"mostrecent"		"1"
		"timestamp"		"1710000000"
	}
}
`
	got, _, err := parseLoginUsers(lowercase)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != 138499704 {
		t.Errorf("steamID3: got %d, want 138499704", got)
	}
}

// One account and no MostRecent=1 is unambiguous — take it. This used
// to be an error, which stranded anyone whose client stopped writing
// the key.
func TestParseLoginUsers_SoleAccountWithoutMostRecent(t *testing.T) {
	const sole = `"users"
{
	"76561198012345678"
	{
		"AccountName"		"alice"
		"MostRecent"		"0"
	}
}
`
	got, source, err := parseLoginUsers(sole)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != 52079950 {
		t.Errorf("steamID3: got %d, want 52079950", got)
	}
	if source != "loginusers.vdf sole account" {
		t.Errorf("source: got %q", source)
	}
}

// Several accounts with nothing to tell them apart: erroring hands the
// decision to the userdata mtime fallback. Guessing here would watch a
// stats file that never changes — indistinguishable from a broken app.
func TestParseLoginUsers_AmbiguousMultiAccount(t *testing.T) {
	const ambiguous = `"users"
{
	"76561198012345678"
	{
		"AccountName"		"alice"
	}
	"76561198098765432"
	{
		"AccountName"		"bob"
	}
}
`
	if _, _, err := parseLoginUsers(ambiguous); err == nil {
		t.Error("expected error on two indistinguishable accounts, got nil")
	}
}

func TestParseLoginUsers_EmptyFile(t *testing.T) {
	if _, _, err := parseLoginUsers(""); err == nil {
		t.Error("expected error on empty file, got nil")
	}
	if _, _, err := parseLoginUsers(`"users" {}`); err == nil {
		t.Error("expected error on empty users block, got nil")
	}
}

func TestParseLoginUsers_RejectsLowSteamID(t *testing.T) {
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
	if _, _, err := parseLoginUsers(bogus); err == nil {
		t.Error("expected error on sub-base steamID, got nil")
	}
}

// ───────────────────────────── userdata\ ───────────────────────────────

func TestReadSteamID3FromUserdata_SoleAccount(t *testing.T) {
	steamPath := t.TempDir()
	mkUserdata(t, steamPath, "49290192", time.Now())
	// Steam's own bookkeeping directories must not be mistaken for accounts.
	mkUserdata(t, steamPath, "0", time.Now())
	mkUserdata(t, steamPath, "ac", time.Now())

	got, source, err := readSteamID3FromUserdata(steamPath)
	if err != nil {
		t.Fatalf("readSteamID3FromUserdata: %v", err)
	}
	if got != 49290192 {
		t.Errorf("steamID3: got %d, want 49290192", got)
	}
	if source != "userdata sole account" {
		t.Errorf("source: got %q", source)
	}
}

func TestReadSteamID3FromUserdata_PicksMostRecentlyModified(t *testing.T) {
	steamPath := t.TempDir()
	now := time.Now()
	mkUserdata(t, steamPath, "11111111", now.Add(-48*time.Hour))
	mkUserdata(t, steamPath, "49290192", now)

	got, source, err := readSteamID3FromUserdata(steamPath)
	if err != nil {
		t.Fatalf("readSteamID3FromUserdata: %v", err)
	}
	if got != 49290192 {
		t.Errorf("steamID3: got %d, want 49290192", got)
	}
	if source != "userdata most recently modified" {
		t.Errorf("source: got %q", source)
	}
}

func TestReadSteamID3FromUserdata_NoAccounts(t *testing.T) {
	steamPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(steamPath, "userdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSteamID3FromUserdata(steamPath); err == nil {
		t.Error("expected error on empty userdata dir, got nil")
	}
}

func mkUserdata(t *testing.T, steamPath, name string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(steamPath, "userdata", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, mod, mod); err != nil {
		t.Fatal(err)
	}
}
