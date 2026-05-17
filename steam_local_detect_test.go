package main

import (
	"testing"
)

// Real-shape appmanifest data sampled from a fresh Steam install. We use
// real bytes (not a hand-crafted snippet) because Steam's VDF has subtle
// formatting quirks — tab indentation, trailing whitespace, line endings
// — that a hand-typed test would smooth over and miss in production.
const sampleAppManifest = `"AppState"
{
	"appid"		"346900"
	"Universe"		"1"
	"name"		"AdVenture Capitalist"
	"StateFlags"		"4"
	"installdir"		"AdVenture Capitalist"
	"LastUpdated"		"1737580800"
	"SizeOnDisk"		"104857600"
	"buildid"		"12345678"
}
`

func TestExtractACFName_TypicalManifest(t *testing.T) {
	got := extractACFName(sampleAppManifest)
	if got != "AdVenture Capitalist" {
		t.Errorf("got %q, want %q", got, "AdVenture Capitalist")
	}
}

func TestExtractACFName_NameMissing(t *testing.T) {
	// Some old/corrupt manifests omit the name field. Don't panic, just
	// return "" so the caller falls back to "Game appid <N>".
	const noName = `"AppState"
{
	"appid"		"346900"
	"installdir"		"X"
}
`
	if got := extractACFName(noName); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractACFName_EmptyInput(t *testing.T) {
	if got := extractACFName(""); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractACFName_HandlesUnicode(t *testing.T) {
	// Tested with a real-world example: japanese titles, accented chars,
	// emoji — Steam stores names as UTF-8 inside the VDF.
	cases := map[string]string{
		`"name"		"Persona 5 Royal"`:                "Persona 5 Royal",
		`"name"		"Witcher 3: Wild Hunt"`:           "Witcher 3: Wild Hunt",
		`"name"		"Ōkami HD"`:                       "Ōkami HD",
		`"name"		"Café Owner Simulator"`:           "Café Owner Simulator",
		`"name"		"パッセンジャーズ"`:                       "パッセンジャーズ",
	}
	for input, want := range cases {
		// Wrap in an AppState block so the regex sees a realistic shape.
		full := "\"AppState\"\n{\n" + input + "\n}\n"
		if got := extractACFName(full); got != want {
			t.Errorf("input=%q: got %q, want %q", input, got, want)
		}
	}
}

func TestExtractACFName_PicksFirstWhenDuplicated(t *testing.T) {
	// Defensive: in the rare case a manifest has two `"name"` keys
	// (e.g. modded ACFs), we take the first one rather than guessing.
	const dup = `"AppState"
{
	"name"		"First"
	"name"		"Second"
}
`
	if got := extractACFName(dup); got != "First" {
		t.Errorf("got %q, want %q", got, "First")
	}
}

// Real-shape libraryfolders.vdf from a multi-library install — one
// default at C:, one secondary at D:. The "0" and "1" headers + path
// values are what we need to extract.
const sampleLibraryFolders = `"libraryfolders"
{
	"0"
	{
		"path"		"C:\\Program Files (x86)\\Steam"
		"label"		""
		"contentid"		"1234567890"
		"totalsize"		"0"
	}
	"1"
	{
		"path"		"D:\\SteamLibrary"
		"label"		"Games"
		"contentid"		"9876543210"
		"totalsize"		"500107862016"
	}
}
`

func TestExtractVDFPaths_MultiLibrary(t *testing.T) {
	got := extractVDFPaths(sampleLibraryFolders)
	want := []string{
		`C:\Program Files (x86)\Steam`,
		`D:\SteamLibrary`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paths, want %d (got: %v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("path %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractVDFPaths_SingleLibrary(t *testing.T) {
	const single = `"libraryfolders"
{
	"0"
	{
		"path"		"C:\\Steam"
	}
}
`
	got := extractVDFPaths(single)
	if len(got) != 1 || got[0] != `C:\Steam` {
		t.Errorf("got %v, want [C:\\Steam]", got)
	}
}

func TestExtractVDFPaths_EmptyFile(t *testing.T) {
	got := extractVDFPaths("")
	if len(got) != 0 {
		t.Errorf("got %v, want empty slice", got)
	}
}

func TestExtractVDFPaths_HandlesForwardSlashes(t *testing.T) {
	// Steam versions <= 2024.10 sometimes write forward slashes for
	// macOS-compatibility reasons even on Windows installs. Normalise.
	const slashed = `"libraryfolders"
{
	"0"
	{
		"path"		"C:/Steam"
	}
}
`
	got := extractVDFPaths(slashed)
	if len(got) != 1 || got[0] != `C:\Steam` {
		t.Errorf("got %v, want [C:\\Steam]", got)
	}
}

func TestExtractVDFPaths_HandlesUNCPath(t *testing.T) {
	// Network drives sometimes appear as UNC paths inside VDF. The
	// double-backslash escape unwraps to a UNC path, which filepath
	// then leaves as-is.
	const unc = `"libraryfolders"
{
	"0"
	{
		"path"		"\\\\nas\\games\\Steam"
	}
}
`
	got := extractVDFPaths(unc)
	if len(got) != 1 || got[0] != `\\nas\games\Steam` {
		t.Errorf("got %v, want [\\\\nas\\games\\Steam]", got)
	}
}
