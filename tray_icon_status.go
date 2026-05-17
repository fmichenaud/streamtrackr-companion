//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// Status icon variants, generated at init() by overlaying a coloured
// dot on the embedded base ICO. Tailwind palette: slate-400 (offline),
// blue-500 (idle), green-500 (active), red-500 (error).
var (
	iconOffline []byte
	iconIdle    []byte
	iconActive  []byte
	iconError   []byte
)

func init() {
	frames, err := decodeIco(trayIcon)
	if err != nil || len(frames) == 0 {
		logf("status icons: decode base ICO failed (%v) — falling back to plain icon for all states", err)
		iconOffline = trayIcon
		iconIdle = trayIcon
		iconActive = trayIcon
		iconError = trayIcon
		return
	}

	statusColors := []struct {
		name string
		c    color.RGBA
		dst  *[]byte
	}{
		{"offline", color.RGBA{0x9C, 0xA3, 0xAF, 0xFF}, &iconOffline},
		{"idle", color.RGBA{0x3B, 0x82, 0xF6, 0xFF}, &iconIdle},
		{"active", color.RGBA{0x22, 0xC5, 0x5E, 0xFF}, &iconActive},
		{"error", color.RGBA{0xEF, 0x44, 0x44, 0xFF}, &iconError},
	}
	for _, s := range statusColors {
		out, err := buildStatusIcon(frames, s.c)
		if err != nil {
			logf("status icons: build %s failed (%v) — using plain icon", s.name, err)
			*s.dst = trayIcon
			continue
		}
		*s.dst = out
	}
}

func buildStatusIcon(frames []image.Image, dotColor color.Color) ([]byte, error) {
	out := make([]image.Image, len(frames))
	for i, f := range frames {
		out[i] = renderOverlay(f, dotColor)
	}
	return encodeIco(out)
}

// ICO decoder. Supports 32-bit DIB and PNG frames (what modern
// Microsoft tooling emits); palette formats bubble up as errors.
//
//   ICONDIR        : reserved(u16=0) | type(u16=1) | count(u16)
//   ICONDIRENTRY×N : w(u8) h(u8) colors(u8) _(u8) planes(u16) bits(u16) size(u32) offset(u32)
//   image data     : PNG (Vista+) or DIB (BITMAPINFOHEADER + pixels)

type icoDirEntry struct {
	Width       uint8
	Height      uint8
	ColorCount  uint8
	Reserved    uint8
	Planes      uint16
	BitCount    uint16
	BytesInRes  uint32
	ImageOffset uint32
}

type bmpInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

func decodeIco(data []byte) ([]image.Image, error) {
	if len(data) < 6 {
		return nil, fmt.Errorf("ICO too short")
	}
	r := bytes.NewReader(data)
	var hdr struct {
		Reserved uint16
		Type     uint16
		Count    uint16
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
		return nil, err
	}
	if hdr.Type != 1 || hdr.Count == 0 {
		return nil, fmt.Errorf("not an ICO (type=%d, count=%d)", hdr.Type, hdr.Count)
	}
	entries := make([]icoDirEntry, hdr.Count)
	for i := range entries {
		if err := binary.Read(r, binary.LittleEndian, &entries[i]); err != nil {
			return nil, fmt.Errorf("dir entry %d: %w", i, err)
		}
	}

	result := make([]image.Image, 0, hdr.Count)
	for i, e := range entries {
		end := uint64(e.ImageOffset) + uint64(e.BytesInRes)
		if end > uint64(len(data)) {
			return nil, fmt.Errorf("frame %d out of bounds (offset=%d size=%d total=%d)", i, e.ImageOffset, e.BytesInRes, len(data))
		}
		sub := data[e.ImageOffset:end]

		// PNG magic — used for the 256×256 frame on Vista+.
		if len(sub) >= 8 && bytes.Equal(sub[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
			img, err := png.Decode(bytes.NewReader(sub))
			if err != nil {
				return nil, fmt.Errorf("frame %d PNG decode: %w", i, err)
			}
			result = append(result, img)
			continue
		}

		// DIB BMP frame.
		img, err := decodeDibFrame(sub)
		if err != nil {
			return nil, fmt.Errorf("frame %d DIB decode: %w", i, err)
		}
		result = append(result, img)
	}
	return result, nil
}

// decodeDibFrame: 32-bpp DIB in ICO has no BITMAPFILEHEADER and a
// doubled height field for the AND mask (ignored — alpha handles it).
func decodeDibFrame(sub []byte) (image.Image, error) {
	if len(sub) < 40 {
		return nil, fmt.Errorf("DIB header too short")
	}
	var dib bmpInfoHeader
	if err := binary.Read(bytes.NewReader(sub[:40]), binary.LittleEndian, &dib); err != nil {
		return nil, err
	}
	if dib.BitCount != 32 {
		return nil, fmt.Errorf("only 32-bpp DIB supported (got %d-bpp)", dib.BitCount)
	}
	w := int(dib.Width)
	h := int(dib.Height) / 2 // halved: format doubles for AND mask
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid DIB dimensions %dx%d", w, h)
	}

	pixels := sub[dib.Size:]
	stride := w * 4
	need := stride * h
	if len(pixels) < need {
		return nil, fmt.Errorf("DIB pixel buffer too short (need %d, got %d)", need, len(pixels))
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	// DIB rows are bottom-up; flip to top-down on copy.
	for y := 0; y < h; y++ {
		src := pixels[y*stride : (y+1)*stride]
		dstY := h - 1 - y
		for x := 0; x < w; x++ {
			b := src[x*4+0]
			g := src[x*4+1]
			r := src[x*4+2]
			a := src[x*4+3]
			img.SetRGBA(x, dstY, color.RGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}

// encodeIco writes a PNG-in-ICO container — every frame PNG-compressed
// then indexed by an ICONDIRENTRY. Vista+.
func encodeIco(frames []image.Image) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames")
	}
	pngs := make([][]byte, len(frames))
	for i, f := range frames {
		var buf bytes.Buffer
		if err := png.Encode(&buf, f); err != nil {
			return nil, fmt.Errorf("encode frame %d: %w", i, err)
		}
		pngs[i] = buf.Bytes()
	}

	var out bytes.Buffer
	// ICONDIR
	_ = binary.Write(&out, binary.LittleEndian, uint16(0))             // reserved
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))             // type = ICO
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(frames)))   // count

	dataOffset := uint32(6 + 16*len(frames))
	for i, f := range frames {
		b := f.Bounds()
		w, h := b.Dx(), b.Dy()
		// ICO encodes 256 as 0 in the single-byte w/h fields.
		wb := uint8(w)
		hb := uint8(h)
		if w >= 256 {
			wb = 0
		}
		if h >= 256 {
			hb = 0
		}
		_ = binary.Write(&out, binary.LittleEndian, wb)
		_ = binary.Write(&out, binary.LittleEndian, hb)
		_ = binary.Write(&out, binary.LittleEndian, uint8(0))  // color count (0 = ≥256)
		_ = binary.Write(&out, binary.LittleEndian, uint8(0))  // reserved
		_ = binary.Write(&out, binary.LittleEndian, uint16(1)) // planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32))
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(pngs[i])))
		_ = binary.Write(&out, binary.LittleEndian, dataOffset)
		dataOffset += uint32(len(pngs[i]))
	}
	for _, p := range pngs {
		out.Write(p)
	}
	return out.Bytes(), nil
}

// renderOverlay copies src and paints a filled coloured disc with a
// 1-pixel dark outline at the bottom-right corner. Disc diameter
// scales to ~33% of the shorter side (min 5px) for legibility at 16×16.
func renderOverlay(src image.Image, c color.Color) image.Image {
	b := src.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, src, b.Min, draw.Src)

	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	diam := side / 3
	if diam < 5 {
		diam = 5
	}
	r := diam / 2
	cx := b.Max.X - r - 1
	cy := b.Max.Y - r - 1

	dark := color.RGBA{0x11, 0x18, 0x27, 0xFF} // slate-900

	// Outline first, fill second — reversing this would paint over the
	// dot's edges.
	for y := -r - 1; y <= r+1; y++ {
		for x := -r - 1; x <= r+1; x++ {
			d2 := x*x + y*y
			if d2 <= (r+1)*(r+1) && d2 > r*r {
				rgba.Set(cx+x, cy+y, dark)
			}
		}
	}
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				rgba.Set(cx+x, cy+y, c)
			}
		}
	}
	return rgba
}
