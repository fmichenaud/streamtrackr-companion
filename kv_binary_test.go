package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Hand-crafted byte streams for the parser tests. Building real Steam
// files is overkill — the parser only cares about the on-the-wire
// structure, and inline assembly of bytes makes failure modes obvious.

// kvBuild is a tiny builder DSL: it concatenates raw bytes into a buffer
// so individual tests stay readable.
type kvBuild struct{ buf bytes.Buffer }

func (b *kvBuild) byte_(v byte)        { b.buf.WriteByte(v) }
func (b *kvBuild) name(s string)       { b.buf.WriteString(s); b.buf.WriteByte(0) }
func (b *kvBuild) int32(v int32)       { _ = binary.Write(&b.buf, binary.LittleEndian, v) }
func (b *kvBuild) uint64(v uint64)     { _ = binary.Write(&b.buf, binary.LittleEndian, v) }
func (b *kvBuild) bytes() []byte       { return b.buf.Bytes() }

func TestParseBinaryKV_SimpleObject(t *testing.T) {
	// "UserGameStats" { "Version" = 1 (int32) }
	var b kvBuild
	b.byte_(kvNone)
	b.name("UserGameStats")
	b.byte_(kvInt32)
	b.name("Version")
	b.int32(1)
	b.byte_(kvEnd) // close UserGameStats

	root, err := parseBinaryKV(b.bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if root.Name != "UserGameStats" {
		t.Errorf("root name: got %q, want %q", root.Name, "UserGameStats")
	}
	if got := root.Child("Version").AsInt(); got != 1 {
		t.Errorf("Version: got %d, want 1", got)
	}
}

func TestParseBinaryKV_NestedBitmaskStat(t *testing.T) {
	// Shape the schema → stats pipeline expects:
	//   "UserGameStats" { "1000" { "Value" = 0x0000000B (int32) } }
	// Bits 0, 1, 3 set → 3 achievements unlocked.
	var b kvBuild
	b.byte_(kvNone)
	b.name("UserGameStats")
	b.byte_(kvNone)
	b.name("1000")
	b.byte_(kvInt32)
	b.name("Value")
	b.int32(0x0B) // 0b1011
	b.byte_(kvEnd) // close "1000"
	b.byte_(kvEnd) // close UserGameStats

	root, err := parseBinaryKV(b.bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	v := root.Child("1000").Child("Value").AsInt()
	if v != 0x0B {
		t.Errorf("Value: got 0x%X, want 0x0B", v)
	}
	// Bit-check semantics — what stats_reader.go will rely on.
	for bit, want := range map[int]bool{0: true, 1: true, 2: false, 3: true, 4: false} {
		got := (v & (1 << bit)) != 0
		if got != want {
			t.Errorf("bit %d: got %v, want %v", bit, got, want)
		}
	}
}

func TestParseBinaryKV_StringValue(t *testing.T) {
	// Schema files store achievement names as kvString.
	var b kvBuild
	b.byte_(kvNone)
	b.name("bits")
	b.byte_(kvNone)
	b.name("0")
	b.byte_(kvString)
	b.name("name")
	b.name("ACH_FIRST_KILL")
	b.byte_(kvEnd)
	b.byte_(kvEnd)

	root, err := parseBinaryKV(b.bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := root.Child("0").Child("name").AsString()
	if got != "ACH_FIRST_KILL" {
		t.Errorf("ach name: got %q, want %q", got, "ACH_FIRST_KILL")
	}
}

func TestParseBinaryKV_UInt64(t *testing.T) {
	// UnlockTime in schemas is sometimes UInt64.
	var b kvBuild
	b.byte_(kvNone)
	b.name("ach")
	b.byte_(kvUInt64)
	b.name("UnlockTime")
	b.uint64(1700000000)
	b.byte_(kvEnd)

	root, err := parseBinaryKV(b.bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := root.Child("UnlockTime").AsInt(); got != 1700000000 {
		t.Errorf("UnlockTime: got %d, want 1700000000", got)
	}
}

func TestParseBinaryKV_MultipleNestedSiblings(t *testing.T) {
	// Real schemas have many sibling stats under "stats". Make sure
	// the parser doesn't lose them on the way through.
	var b kvBuild
	b.byte_(kvNone)
	b.name("stats")
	for i, name := range []string{"100", "200", "300"} {
		b.byte_(kvNone)
		b.name(name)
		b.byte_(kvInt32)
		b.name("Value")
		b.int32(int32(i + 1))
		b.byte_(kvEnd)
	}
	b.byte_(kvEnd) // close stats

	root, err := parseBinaryKV(b.bytes())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(root.Children); got != 3 {
		t.Fatalf("stats children: got %d, want 3", got)
	}
	for i, name := range []string{"100", "200", "300"} {
		want := int64(i + 1)
		if got := root.Child(name).Child("Value").AsInt(); got != want {
			t.Errorf("stat %q value: got %d, want %d", name, got, want)
		}
	}
}

func TestParseBinaryKV_RejectsUnknownType(t *testing.T) {
	// 0xFE is unassigned — must error rather than silently skip data,
	// otherwise we'd misparse the rest of the file as garbage.
	var b kvBuild
	b.byte_(kvNone)
	b.name("root")
	b.byte_(0xFE)
	b.name("mystery")

	if _, err := parseBinaryKV(b.bytes()); err == nil {
		t.Error("expected error on unknown type byte, got nil")
	}
}

func TestParseBinaryKV_RejectsEmptyDocument(t *testing.T) {
	if _, err := parseBinaryKV(nil); err == nil {
		t.Error("expected error on empty input, got nil")
	}
}

func TestParseBinaryKV_RejectsDeeplyNested(t *testing.T) {
	// Build a doc nested kvMaxDepth+10 levels deep. Without the depth
	// guard this would blow the goroutine stack on a crafted file
	// dropped into appcache/stats/ by malware or another local user.
	var b kvBuild
	depth := kvMaxDepth + 10
	for i := 0; i < depth; i++ {
		b.byte_(kvNone)
		b.name("n")
	}
	for i := 0; i < depth; i++ {
		b.byte_(kvEnd)
	}
	if _, err := parseBinaryKV(b.bytes()); err == nil {
		t.Errorf("expected error past %d levels of nesting, got nil", kvMaxDepth)
	}
}

func TestKVNode_ChildNilSafe(t *testing.T) {
	// Chained walks like root.Child("a").Child("b").AsInt() must not
	// crash when "a" is missing — the call site stays clean of nil
	// checks at every hop.
	var n *kvNode
	if n.Child("anything") != nil {
		t.Error("Child on nil should return nil")
	}
	if n.AsInt() != 0 {
		t.Error("AsInt on nil should return 0")
	}
	if n.AsString() != "" {
		t.Error("AsString on nil should return empty")
	}
}

func TestKVNode_AsIntParsesStringDigits(t *testing.T) {
	// Schema "type" fields appear sometimes as kvInt32, sometimes as
	// kvString ("4"). AsInt should normalise.
	n := &kvNode{Type: kvString, Str: "42"}
	if got := n.AsInt(); got != 42 {
		t.Errorf("AsInt of string '42': got %d, want 42", got)
	}
}
