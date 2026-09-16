// fakesteam builds, and then mutates, the Steam files the companion
// watches — schema, per-user stats, loginusers.vdf — so the whole watch
// loop can be exercised on any OS, with no Steam and no game.
//
// It pairs with `readSteamPath`, which honours $STEAMPATH off Windows, and
// with `--cli -appid`, which skips the registry-based game detection. See
// tools/README.md for the scenarios.
//
// Everything lives in one ACHIEVEMENTS stat as a bitmask, one bit per
// achievement — the shape real games use.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BinaryKV type bytes, mirrored from kv_binary.go (this is a separate
// package on purpose: the harness must not be able to "fix" a parser bug
// by sharing code with the parser under test).
const (
	kvNone   byte = 0x00
	kvString byte = 0x01
	kvInt32  byte = 0x02
	kvEnd    byte = 0x08
)

const statTypeAchievements = 4

func main() {
	root := flag.String("root", "./steam", "Steam tree to build (point $STEAMPATH at it)")
	appid := flag.Uint("appid", 440, "Steam appid")
	steamID64 := flag.Uint64("steamid", 76561198000000042, "Steam ID64 of the fake signed-in account")
	namesF := flag.String("names", "ACH_A,ACH_B,ACH_C,ACH_D", "Achievement apiNames, in bit order")
	flag.Usage = usage
	flag.Parse()

	names := strings.Split(*namesF, ",")
	id3 := uint32(*steamID64 & 0xFFFFFFFF)
	statsPath := filepath.Join(*root, "appcache", "stats",
		fmt.Sprintf("UserGameStats_%d_%d.bin", id3, *appid))

	unlocked := flag.Args()
	if len(unlocked) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, unlocked := unlocked[0], unlocked[1:]

	switch cmd {
	case "init":
		write(filepath.Join(*root, "appcache", "stats",
			fmt.Sprintf("UserGameStatsSchema_%d.bin", *appid)), schemaBytes(uint32(*appid), names))
		write(filepath.Join(*root, "config", "loginusers.vdf"), loginUsers(*steamID64))
		write(statsPath, statsBytes(maskOf(names, unlocked)))
		fmt.Printf("steam tree at %s — appid=%d id3=%d unlocked=%v\n", *root, *appid, id3, unlocked)

	case "set":
		write(statsPath, statsBytes(maskOf(names, unlocked)))
		fmt.Printf("unlocked = %v\n", unlocked)

	// The two shapes Steam leaves behind when caught mid-rewrite. Both
	// read as "every achievement is gone" if anything parses them
	// leniently, which is what the confirmation read exists to catch.
	case "truncate":
		full := statsBytes(maskOf(names, unlocked))
		write(statsPath, full[:len(full)/2])
		fmt.Println("stats file truncated mid-write")

	case "empty":
		write(statsPath, nil)
		fmt.Println("stats file emptied")

	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fakesteam builds a Steam tree the companion can watch.

  fakesteam [flags] init [unlocked...]      create schema + stats + loginusers.vdf
  fakesteam [flags] set  [unlocked...]      rewrite stats (unlock / re-lock)
  fakesteam [flags] truncate [unlocked...]  write half a stats file
  fakesteam [flags] empty                   write a zero-byte stats file

Flags:
`)
	flag.PrintDefaults()
}

func schemaBytes(appid uint32, names []string) []byte {
	var b bytes.Buffer
	open(&b, strconv.FormatUint(uint64(appid), 10))
	open(&b, "stats")
	open(&b, "1") // statID
	b.WriteByte(kvInt32)
	cstr(&b, "type")
	_ = binary.Write(&b, binary.LittleEndian, int32(statTypeAchievements))
	b.WriteByte(kvString)
	cstr(&b, "name")
	cstr(&b, "Achievements")
	open(&b, "bits")
	for i, n := range names {
		open(&b, strconv.Itoa(i))
		b.WriteByte(kvString)
		cstr(&b, "name")
		cstr(&b, n)
		b.WriteByte(kvEnd)
	}
	b.WriteByte(kvEnd) // bits
	b.WriteByte(kvEnd) // statID
	b.WriteByte(kvEnd) // stats
	b.WriteByte(kvEnd) // appid
	return b.Bytes()
}

func statsBytes(mask int32) []byte {
	var b bytes.Buffer
	open(&b, "UserGameStats")
	open(&b, "1") // statID
	b.WriteByte(kvInt32)
	cstr(&b, "data")
	_ = binary.Write(&b, binary.LittleEndian, mask)
	b.WriteByte(kvEnd)
	b.WriteByte(kvEnd)
	return b.Bytes()
}

// loginUsers is the minimum resolveSteamID3 accepts once the registry
// lookup has failed, which it always does off Windows.
func loginUsers(steamID64 uint64) []byte {
	return []byte(fmt.Sprintf("\"users\"\n{\n\t\"%d\"\n\t{\n\t\t\"MostRecent\"\t\"1\"\n\t\t\"Timestamp\"\t\"1700000000\"\n\t}\n}\n", steamID64))
}

func maskOf(names, unlocked []string) int32 {
	var m int32
	for _, want := range unlocked {
		found := false
		for i, n := range names {
			if n == want {
				m |= 1 << i
				found = true
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "warning: %q is not in -names, ignored\n", want)
		}
	}
	return m
}

func open(b *bytes.Buffer, name string) {
	b.WriteByte(kvNone)
	cstr(b, name)
}

func cstr(b *bytes.Buffer, s string) {
	b.WriteString(s)
	b.WriteByte(0)
}

func write(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		panic(err)
	}
}
