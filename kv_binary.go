package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// Parser for Valve's "BinaryKV" format (binary VDF) — used by every
// appcache file Steam writes locally. Layout, recursive:
//
//   type:   u8
//   name:   null-terminated UTF-8 (skipped when type == kvEnd)
//   value:  per-type, little-endian
//
// kvNone opens a nested object; children follow until a kvEnd byte.

const (
	kvNone   byte = 0x00 // nested object
	kvString byte = 0x01 // null-terminated UTF-8
	kvInt32  byte = 0x02
	kvFloat  byte = 0x03
	kvPtr    byte = 0x04
	kvWStr   byte = 0x05 // null-terminated UTF-16LE
	kvColor  byte = 0x06
	kvUInt64 byte = 0x07
	kvEnd    byte = 0x08 // closes a kvNone block
	kvInt64  byte = 0x0A
)

type kvNode struct {
	Name     string
	Type     byte
	Str      string
	Int      int64
	Float    float32
	Children []*kvNode
}

// Cap nested-object recursion. Real Steam schemas hit ~5 levels; 32 is
// 6× safety margin while guarding against a crafted file in
// appcache/stats/ blowing the goroutine stack on parse.
const kvMaxDepth = 32

// parseBinaryKV decodes a complete document and returns its single
// top-level node.
func parseBinaryKV(data []byte) (*kvNode, error) {
	r := bytes.NewReader(data)
	root := &kvNode{Type: kvNone}
	if err := readKVChildren(r, root, 0); err != nil {
		return nil, err
	}
	if len(root.Children) == 0 {
		return nil, fmt.Errorf("kv: empty document")
	}
	return root.Children[0], nil
}

func readKVChildren(r *bytes.Reader, parent *kvNode, depth int) error {
	if depth > kvMaxDepth {
		return fmt.Errorf("kv: max nesting depth %d exceeded", kvMaxDepth)
	}
	for {
		typ, err := r.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("kv: read type: %w", err)
		}
		if typ == kvEnd {
			return nil
		}

		name, err := readCString(r)
		if err != nil {
			return fmt.Errorf("kv: read name: %w", err)
		}
		child := &kvNode{Name: name, Type: typ}

		switch typ {
		case kvNone:
			if err := readKVChildren(r, child, depth+1); err != nil {
				return fmt.Errorf("kv: children of %q: %w", name, err)
			}
		case kvString:
			s, err := readCString(r)
			if err != nil {
				return fmt.Errorf("kv: string value of %q: %w", name, err)
			}
			child.Str = s
		case kvInt32, kvPtr, kvColor:
			var v uint32
			if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
				return fmt.Errorf("kv: int32 value of %q: %w", name, err)
			}
			child.Int = int64(int32(v))
		case kvFloat:
			if err := binary.Read(r, binary.LittleEndian, &child.Float); err != nil {
				return fmt.Errorf("kv: float value of %q: %w", name, err)
			}
		case kvUInt64, kvInt64:
			var v int64
			if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
				return fmt.Errorf("kv: int64 value of %q: %w", name, err)
			}
			child.Int = v
		case kvWStr:
			s, err := readWCString(r)
			if err != nil {
				return fmt.Errorf("kv: wstring value of %q: %w", name, err)
			}
			child.Str = s
		default:
			return fmt.Errorf("kv: unknown type 0x%02X for key %q", typ, name)
		}

		parent.Children = append(parent.Children, child)
	}
}

func readCString(r *bytes.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == 0 {
			break
		}
		buf = append(buf, b)
	}
	return string(buf), nil
}

func readWCString(r *bytes.Reader) (string, error) {
	var runes []uint16
	for {
		var u uint16
		if err := binary.Read(r, binary.LittleEndian, &u); err != nil {
			return "", err
		}
		if u == 0 {
			break
		}
		runes = append(runes, u)
	}
	out := make([]rune, len(runes))
	for i, u := range runes {
		out[i] = rune(u)
	}
	return string(out), nil
}

// Child returns the first direct child with the given name, or nil.
// Nil-safe so chained walks like root.Child("a").Child("b") don't
// require nil checks at every hop.
func (n *kvNode) Child(name string) *kvNode {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// AsInt returns the integer value, or 0 for nil / non-numeric types.
// Strings of digits are parsed (some Valve files store ints as
// strings).
func (n *kvNode) AsInt() int64 {
	if n == nil {
		return 0
	}
	switch n.Type {
	case kvInt32, kvUInt64, kvInt64, kvPtr, kvColor:
		return n.Int
	case kvString:
		var v int64
		_, err := fmt.Sscanf(n.Str, "%d", &v)
		if err != nil {
			return 0
		}
		return v
	}
	return 0
}

func (n *kvNode) AsString() string {
	if n == nil {
		return ""
	}
	if n.Type == kvString || n.Type == kvWStr {
		return n.Str
	}
	return ""
}
