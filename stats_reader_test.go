package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Hand-built schema and stats byte streams. Real Steam files would be
// much larger (dozens of stats × multiple bits) but the parser cares
// about shape, not size.

// buildSchemaBytes serialises a single ACHIEVEMENTS stat with the given
// (bit → apiName) mapping, wrapped in the canonical `"<appid>" / "stats"`
// envelope Steam writes for real games.
func buildSchemaBytes(appid uint32, statID uint32, bits map[uint32]string) []byte {
	var b bytes.Buffer
	// Root: "<appid>" {
	b.WriteByte(kvNone)
	b.WriteString(formatUint(appid))
	b.WriteByte(0)
	//   "stats" {
	b.WriteByte(kvNone)
	b.WriteString("stats")
	b.WriteByte(0)
	//     "<statID>" {
	b.WriteByte(kvNone)
	b.WriteString(formatUint(statID))
	b.WriteByte(0)
	//       "type" = 4 (ACHIEVEMENTS)
	b.WriteByte(kvInt32)
	b.WriteString("type")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, int32(statTypeAchievements))
	//       "name" = "Achievements"
	b.WriteByte(kvString)
	b.WriteString("name")
	b.WriteByte(0)
	b.WriteString("Achievements")
	b.WriteByte(0)
	//       "bits" {
	b.WriteByte(kvNone)
	b.WriteString("bits")
	b.WriteByte(0)
	for bit, name := range bits {
		//   "<bit>" {
		b.WriteByte(kvNone)
		b.WriteString(formatUint(bit))
		b.WriteByte(0)
		//     "name" = "<apiName>"
		b.WriteByte(kvString)
		b.WriteString("name")
		b.WriteByte(0)
		b.WriteString(name)
		b.WriteByte(0)
		//   }
		b.WriteByte(kvEnd)
	}
	//       } // close bits
	b.WriteByte(kvEnd)
	//     } // close <statID>
	b.WriteByte(kvEnd)
	//   } // close stats
	b.WriteByte(kvEnd)
	// } // close root
	b.WriteByte(kvEnd)
	return b.Bytes()
}

// buildStatsBytes serialises a UserGameStats document with one statID
// holding the given bitmask Value.
func buildStatsBytes(statID uint32, value int32) []byte {
	var b bytes.Buffer
	b.WriteByte(kvNone)
	b.WriteString("UserGameStats")
	b.WriteByte(0)
	b.WriteByte(kvNone)
	b.WriteString(formatUint(statID))
	b.WriteByte(0)
	b.WriteByte(kvInt32)
	b.WriteString("data")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, value)
	b.WriteByte(kvEnd) // close statID
	b.WriteByte(kvEnd) // close UserGameStats
	return b.Bytes()
}

func formatUint(v uint32) string {
	// We only need positive base-10 — strconv would pull in fmt's full
	// machinery; this keeps the test file self-contained.
	if v == 0 {
		return "0"
	}
	var buf [11]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func TestExtractSlots_SingleStatThreeAchievements(t *testing.T) {
	data := buildSchemaBytes(346900, 1, map[uint32]string{
		0: "ACH_FIRST_KILL",
		1: "ACH_HEADSHOT",
		2: "ACH_VETERAN",
	})
	root, err := parseBinaryKV(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	slots := extractSlots(root)
	if len(slots) != 3 {
		t.Fatalf("slot count: got %d, want 3", len(slots))
	}
	// Verify each slot resolves to its expected (statID, bit, name).
	// Map iteration order is non-deterministic on the build side, so
	// look up by name rather than by index.
	byName := map[string]achievementSlot{}
	for _, s := range slots {
		byName[s.APIName] = s
	}
	for _, want := range []achievementSlot{
		{APIName: "ACH_FIRST_KILL", StatID: 1, Bit: 0},
		{APIName: "ACH_HEADSHOT", StatID: 1, Bit: 1},
		{APIName: "ACH_VETERAN", StatID: 1, Bit: 2},
	} {
		got, ok := byName[want.APIName]
		if !ok {
			t.Errorf("missing slot for %q", want.APIName)
			continue
		}
		if got != want {
			t.Errorf("slot for %q: got %+v, want %+v", want.APIName, got, want)
		}
	}
}

func TestExtractSlots_IgnoresNonAchievementStats(t *testing.T) {
	// Stat with type=1 (INT, regular counter) — must NOT produce
	// any slots. Build a custom doc since the helper hard-codes type=4.
	var b bytes.Buffer
	b.WriteByte(kvNone)
	b.WriteString("346900")
	b.WriteByte(0)
	b.WriteByte(kvNone)
	b.WriteString("stats")
	b.WriteByte(0)
	b.WriteByte(kvNone)
	b.WriteString("1")
	b.WriteByte(0)
	b.WriteByte(kvInt32)
	b.WriteString("type")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, int32(statTypeInt))
	b.WriteByte(kvString)
	b.WriteString("name")
	b.WriteByte(0)
	b.WriteString("kill_count")
	b.WriteByte(0)
	b.WriteByte(kvEnd) // close stat
	b.WriteByte(kvEnd) // close stats
	b.WriteByte(kvEnd) // close root

	root, err := parseBinaryKV(b.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := extractSlots(root); len(got) != 0 {
		t.Errorf("expected 0 slots from non-achievement stat, got %d", len(got))
	}
}

func TestExtractStatValues_ReturnsAllValues(t *testing.T) {
	data := buildStatsBytes(1, 0x0F)
	root, err := parseBinaryKV(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	vals := extractStatValues(root)
	if got := vals[1]; got != 0x0F {
		t.Errorf("stat 1: got 0x%X, want 0x0F", got)
	}
}

func TestExtractStatValues_RealFileShape(t *testing.T) {
	// Real UserGameStats_<sid>_<appid>.bin files use a "cache" root
	// with int32 "crc" and "PendingChanges" siblings *alongside* the
	// numeric stat children. Pin that extractStatValues ignores those
	// non-numeric-keyed siblings rather than crashing or polluting
	// the result map.
	var b bytes.Buffer
	b.WriteByte(kvNone)
	b.WriteString("cache")
	b.WriteByte(0)
	// crc — int32 sibling, NOT a stat.
	b.WriteByte(kvInt32)
	b.WriteString("crc")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, int32(1007086245))
	// PendingChanges — int32 sibling, NOT a stat.
	b.WriteByte(kvInt32)
	b.WriteString("PendingChanges")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, int32(0))
	// Real stat at statID=1 with data + AchievementTimes (which we
	// ignore — only "data" matters for the bitmask).
	b.WriteByte(kvNone)
	b.WriteString("1")
	b.WriteByte(0)
	b.WriteByte(kvInt32)
	b.WriteString("data")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, int32(-1)) // all 32 bits set
	b.WriteByte(kvNone)
	b.WriteString("AchievementTimes")
	b.WriteByte(0)
	b.WriteByte(kvInt32)
	b.WriteString("0")
	b.WriteByte(0)
	_ = binary.Write(&b, binary.LittleEndian, int32(1700000000))
	b.WriteByte(kvEnd) // close AchievementTimes
	b.WriteByte(kvEnd) // close stat "1"
	b.WriteByte(kvEnd) // close cache

	root, err := parseBinaryKV(b.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	vals := extractStatValues(root)
	if got := vals[1]; got != -1 {
		t.Errorf("stat 1: got 0x%X, want -1 (all bits set)", got)
	}
	// crc and PendingChanges must NOT appear in the map — their keys
	// aren't numeric so they're not statIDs.
	if _, present := vals[0]; present {
		t.Error("non-numeric sibling 'crc' or 'PendingChanges' leaked into stat map")
	}
	if len(vals) != 1 {
		t.Errorf("expected exactly 1 stat in map, got %d (%v)", len(vals), vals)
	}
}

func TestComputeUnlocked_BitMaskMath(t *testing.T) {
	slots := []achievementSlot{
		{APIName: "A", StatID: 1, Bit: 0},
		{APIName: "B", StatID: 1, Bit: 1},
		{APIName: "C", StatID: 1, Bit: 2},
		{APIName: "D", StatID: 1, Bit: 3},
	}
	// 0b1010 → A locked, B unlocked, C locked, D unlocked
	got := computeUnlocked(slots, map[uint32]int32{1: 0x0A})
	want := map[string]bool{"A": false, "B": true, "C": false, "D": true}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %v, want %v (mask was 0x0A)", k, got[k], w)
		}
	}
}

func TestComputeUnlocked_MissingStatTreatedAsZero(t *testing.T) {
	// A schema slot pointing at a statID that doesn't appear in the
	// stats file means the user has never unlocked anything from that
	// bank — return all false rather than panicking.
	slots := []achievementSlot{
		{APIName: "X", StatID: 99, Bit: 0},
		{APIName: "Y", StatID: 99, Bit: 5},
	}
	got := computeUnlocked(slots, map[uint32]int32{})
	if got["X"] || got["Y"] {
		t.Errorf("expected all-locked when stat absent, got %v", got)
	}
}

func TestComputeUnlocked_TopBitSet(t *testing.T) {
	// Bit 31 maps to sign bit when the int32 is cast naively. We do
	// the math through uint32 so bit 31 unlocked == true rather than
	// "negative number" weirdness.
	slots := []achievementSlot{{APIName: "TOP", StatID: 1, Bit: 31}}
	got := computeUnlocked(slots, map[uint32]int32{1: -1}) // -1 == 0xFFFFFFFF
	if !got["TOP"] {
		t.Errorf("bit 31 (top of int32) should read as unlocked when value=0xFFFFFFFF, got false")
	}
}

func TestExtractSlots_NoTypeField(t *testing.T) {
	// Some indies (e.g. Megabonk, appid 3405340) omit the "type" field
	// inside ACHIEVEMENTS stats, or encode it as the string
	// "ACHIEVEMENTS" instead of the int 4. extractSlots must rely on
	// the "bits" child being present, not on a specific "type" value.
	var b bytes.Buffer
	b.WriteByte(kvNone)
	b.WriteString("3405340")
	b.WriteByte(0)
	b.WriteByte(kvNone)
	b.WriteString("stats")
	b.WriteByte(0)
	b.WriteByte(kvNone)
	b.WriteString("1")
	b.WriteByte(0)
	// No "type" field at all — pre-Megabonk parser would skip here.
	b.WriteByte(kvNone)
	b.WriteString("bits")
	b.WriteByte(0)
	b.WriteByte(kvNone)
	b.WriteString("0")
	b.WriteByte(0)
	b.WriteByte(kvString)
	b.WriteString("name")
	b.WriteByte(0)
	b.WriteString("a_battery")
	b.WriteByte(0)
	b.WriteByte(kvEnd) // close bit "0"
	b.WriteByte(kvEnd) // close bits
	// Type comes AFTER bits, as a string — matches Megabonk's order.
	b.WriteByte(kvString)
	b.WriteString("type")
	b.WriteByte(0)
	b.WriteString("ACHIEVEMENTS")
	b.WriteByte(0)
	b.WriteByte(kvEnd) // close stat "1"
	b.WriteByte(kvEnd) // close stats
	b.WriteByte(kvEnd) // close root

	root, err := parseBinaryKV(b.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	slots := extractSlots(root)
	if len(slots) != 1 {
		t.Fatalf("slot count: got %d, want 1", len(slots))
	}
	if slots[0].APIName != "a_battery" {
		t.Errorf("apiName: got %q, want %q", slots[0].APIName, "a_battery")
	}
}

func TestExtractSlots_NoStatsBlock(t *testing.T) {
	// Schemas for games without achievements (mod tools, dedicated
	// servers etc.) still write a "<appid>" wrapper but no "stats"
	// child. Must return nil rather than panic.
	var b bytes.Buffer
	b.WriteByte(kvNone)
	b.WriteString("346900")
	b.WriteByte(0)
	b.WriteByte(kvEnd) // empty root

	root, err := parseBinaryKV(b.Bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := extractSlots(root); got != nil {
		t.Errorf("expected nil slots from empty schema, got %v", got)
	}
}
