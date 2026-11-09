// Package lcechunk reads and writes the chunk column payload of the console (LCE) chunk packet (id 0x33): zlib, then a 0xFF
// run-length pass, then a packed storage of one 16x256x16 column. The layout below was read from the 400 chunk packets in the
// 2026-11-07 Wii U capture (docs/provenance.md, rows C1-C9); nothing here comes from any other source.
//
//	blocks      u32 BE length of the rest of this part, 4 x u16 LE counters, 16 x u16 LE section start (in 256-byte pages),
//	            16 x u8 section page count, then the pages. A section (16 high) is a 128-byte index of 64 u16 LE tile entries
//	            (4x4x4 voxels; entry = mode<<12 | offset in 4-byte units) followed by the tile data, padded to whole pages.
//	            Voxel value = block id<<4 | data. Mode 0 uniform (the entry itself is the value), 2/4/6 = 1/2/3 bit planes with a
//	            2/4/8-entry u16 palette (first-use order, 0xFFFF fill), 14 = 64 raw u16. Order everywhere: x slowest, z, y fastest.
//	block light two stores, then sky light two stores (lower and upper 128 layers): a 128-byte layer index (0x80 all zero layer,
//	            0x81 all 0xFF layer, else a page number) and the 128-byte pages (16x16 nibbles, x slowest, low nibble first)
//	biomes      256 bytes, z*16+x
package lcechunk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	W      = 16
	H      = 256
	Voxels = W * W * H
)

// Idx is the position of block (x, y, z) in a Column's arrays: x slowest, y fastest (the storage order).
func Idx(x, y, z int) int { return (x*W+z)*H + y }

// Column is one decoded column.
type Column struct {
	Block      [Voxels]uint16 // id<<4 | data
	BlockLight [Voxels]uint8  // 0..15
	SkyLight   [Voxels]uint8  // 0..15
	Biome      [W * W]uint8   // z*16+x
}

// Unpack decodes the compressed payload of a chunk packet (the bytes after the size word).
func Unpack(comp []byte) (*Column, error) {
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	rle, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	raw, err := unrle(rle)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Pack encodes c into the compressed payload of a chunk packet.
func Pack(c *Column) []byte {
	var out bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&out, zlib.DefaultCompression)
	zw.Write(rle(Serialize(c)))
	zw.Close()
	return out.Bytes()
}

func unrle(z []byte) ([]byte, error) {
	out := make([]byte, 0, len(z)*2)
	for i := 0; i < len(z); {
		c := z[i]
		i++
		if c != 0xFF {
			out = append(out, c)
			continue
		}
		if i >= len(z) {
			return nil, errors.New("lcechunk: run-length escape at end")
		}
		n := int(z[i]) + 1
		i++
		if n <= 3 {
			for ; n > 0; n-- {
				out = append(out, 0xFF)
			}
			continue
		}
		if i >= len(z) {
			return nil, errors.New("lcechunk: run-length value missing")
		}
		v := z[i]
		i++
		for ; n > 0; n-- {
			out = append(out, v)
		}
	}
	return out, nil
}

// rle is the inverse: runs of 4..256 equal bytes become FF count-1 value, a lone FF becomes FF 00 (n<=3 means "n x FF").
func rle(b []byte) []byte {
	out := make([]byte, 0, len(b)/2)
	for i := 0; i < len(b); {
		j := i
		for j < len(b) && b[j] == b[i] && j-i < 256 {
			j++
		}
		n := j - i
		switch {
		case n >= 4:
			out = append(out, 0xFF, byte(n-1), b[i])
		case b[i] == 0xFF:
			out = append(out, 0xFF, byte(n-1))
		default:
			for k := 0; k < n; k++ {
				out = append(out, b[i])
			}
		}
		i = j
	}
	return out
}

// modeBits and modeSize are the tile modes this package reads and writes.
var modeBits = map[int]int{2: 1, 4: 2, 6: 3, 8: 4}
var modeSize = map[int]int{2: 12, 4: 24, 6: 40, 8: 64, 14: 128}

// Parse decodes the unpacked (run-length expanded) column.
func Parse(raw []byte) (*Column, error) {
	c := new(Column)
	if len(raw) < 60+256 {
		return nil, errors.New("lcechunk: short column")
	}
	body := int(binary.BigEndian.Uint32(raw))
	if 12+body > len(raw) {
		return nil, errors.New("lcechunk: block part overruns")
	}
	var start [16]int
	var cnt [16]int
	for s := 0; s < 16; s++ {
		start[s] = int(binary.LittleEndian.Uint16(raw[12+2*s:]))
		cnt[s] = int(raw[44+s])
	}
	for s := 0; s < 16; s++ {
		if cnt[s] == 0 {
			continue
		}
		lo := 0x3c + start[s]*256
		if lo+cnt[s]*256 > len(raw) {
			return nil, fmt.Errorf("lcechunk: section %d overruns", s)
		}
		if err := parseSection(c, s, raw[lo:lo+cnt[s]*256]); err != nil {
			return nil, err
		}
	}
	pos := 12 + body
	for i, dst := range []*[Voxels]uint8{&c.BlockLight, &c.BlockLight, &c.SkyLight, &c.SkyLight} {
		n, err := parseLight(raw[pos:], dst, (i%2)*128)
		if err != nil {
			return nil, err
		}
		pos += n
	}
	if len(raw)-pos != 256 {
		return nil, fmt.Errorf("lcechunk: %d bytes after the light stores, want 256 biome bytes", len(raw)-pos)
	}
	copy(c.Biome[:], raw[pos:])
	return c, nil
}

func parseSection(c *Column, s int, sec []byte) error {
	for ti := 0; ti < 64; ti++ {
		e := binary.LittleEndian.Uint16(sec[ti*2:])
		mode, off := int(e>>12), int(e&0xfff)*4
		var v [64]uint16
		switch mode {
		case 0:
			for i := range v {
				v[i] = e
			}
		case 14:
			if 128+off+128 > len(sec) {
				return errors.New("lcechunk: tile overruns")
			}
			for i := range v {
				v[i] = binary.LittleEndian.Uint16(sec[128+off+2*i:])
			}
		case 2, 4, 6, 8:
			bits := modeBits[mode]
			if 128+off+modeSize[mode] > len(sec) {
				return errors.New("lcechunk: tile overruns")
			}
			t := sec[128+off:]
			pal := 2 << (bits - 1)
			planes := t[2*pal:]
			for i := range v {
				k := 0
				for b := 0; b < bits; b++ {
					k |= int(planes[b*8+i/8]>>(7-i%8)&1) << b
				}
				v[i] = binary.LittleEndian.Uint16(t[2*k:])
			}
		default:
			return fmt.Errorf("lcechunk: unsupported tile mode %d", mode)
		}
		tx, tz, ty := ti/16, ti/4%4, ti%4
		for i, val := range v {
			c.Block[Idx(tx*4+i/16, s*16+ty*4+i%4, tz*4+i/4%4)] = val
		}
	}
	return nil
}

func parseLight(b []byte, dst *[Voxels]uint8, y0 int) (int, error) {
	if len(b) < 128 {
		return 0, errors.New("lcechunk: short light store")
	}
	idx := b[:128]
	pages := 0
	for _, v := range idx {
		if v < 0x80 {
			pages++
		}
	}
	if len(b) < 128+pages*128 {
		return 0, errors.New("lcechunk: light pages overrun")
	}
	for l := 0; l < 128; l++ {
		v := idx[l]
		if v == 0x80 {
			continue
		}
		for i := 0; i < 256; i++ {
			var n uint8 = 15
			if v < 0x80 {
				by := b[128+int(v)*128+i/2]
				n = by & 15
				if i%2 == 1 {
					n = by >> 4
				}
			}
			dst[Idx(i/16, y0+l, i%16)] = n
		}
	}
	return 128 + pages*128, nil
}

// Serialize builds the unpacked column (before run-length and zlib).
func Serialize(c *Column) []byte {
	var cnt [16]int
	var start [16]int
	var secs [][]byte
	total := 0
	for s := 0; s < 16; s++ {
		sec := buildSection(c, s)
		start[s] = total
		if sec != nil {
			cnt[s] = len(sec) / 256
			total += cnt[s]
			secs = append(secs, sec)
		}
	}
	out := make([]byte, 60, 60+total*256+4*128+256)
	binary.BigEndian.PutUint32(out, uint32(48+total*256))
	pages := func(n int) uint16 { // counter: pages/2 rounded up, bit 15 when the page count is even
		v := uint16((n + 1) / 2)
		if n%2 == 0 {
			v |= 0x8000
		}
		return v
	}
	var stores [4][]byte
	for i, src := range []*[Voxels]uint8{&c.BlockLight, &c.BlockLight, &c.SkyLight, &c.SkyLight} {
		stores[i] = buildLight(src, (i%2)*128)
	}
	for i := 0; i < 4; i++ {
		n := 0
		for _, v := range stores[i][:128] {
			if v < 0x80 {
				n++
			}
		}
		binary.LittleEndian.PutUint16(out[4+2*i:], pages(n))
	}
	for s := 0; s < 16; s++ {
		binary.LittleEndian.PutUint16(out[12+2*s:], uint16(start[s]))
		out[44+s] = byte(cnt[s])
	}
	for _, sec := range secs {
		out = append(out, sec...)
	}
	for _, st := range stores {
		out = append(out, st...)
	}
	return append(out, c.Biome[:]...)
}

func buildSection(c *Column, s int) []byte {
	idx := make([]byte, 128)
	var data []byte
	allZero := true
	for ti := 0; ti < 64; ti++ {
		tx, tz, ty := ti/16, ti/4%4, ti%4
		var v [64]uint16
		var pal []uint16
		var k [64]int
		for i := range v {
			v[i] = c.Block[Idx(tx*4+i/16, s*16+ty*4+i%4, tz*4+i/4%4)]
			if v[i] != 0 {
				allZero = false
			}
			p := -1
			for j, q := range pal {
				if q == v[i] {
					p = j
					break
				}
			}
			if p < 0 {
				p = len(pal)
				pal = append(pal, v[i])
			}
			k[i] = p
		}
		var e uint16
		switch n := len(pal); {
		case n == 1:
			e = pal[0] // mode 0: the entry is the value (ids stay below 0x100, so the top nibble is free)
		case n <= 8:
			mode, bits := 2, 1
			if n > 2 {
				mode, bits = 4, 2
			}
			if n > 4 {
				mode, bits = 6, 3
			}
			e = uint16(mode)<<12 | uint16(len(data)/4)
			t := make([]byte, modeSize[mode])
			for j := 0; j < 2<<(bits-1); j++ {
				p := uint16(0xFFFF)
				if j < n {
					p = pal[j]
				}
				binary.LittleEndian.PutUint16(t[2*j:], p)
			}
			planes := t[2*(2<<(bits-1)):]
			for i := range v {
				for b := 0; b < bits; b++ {
					if k[i]>>b&1 == 1 {
						planes[b*8+i/8] |= 1 << (7 - i%8)
					}
				}
			}
			data = append(data, t...)
		default:
			e = 14<<12 | uint16(len(data)/4)
			for _, x := range v {
				data = binary.LittleEndian.AppendUint16(data, x)
			}
		}
		binary.LittleEndian.PutUint16(idx[ti*2:], e)
	}
	if allZero {
		return nil
	}
	sec := append(idx, data...)
	for len(sec)%256 != 0 {
		sec = append(sec, 0)
	}
	return sec
}

func buildLight(src *[Voxels]uint8, y0 int) []byte {
	idx := make([]byte, 128)
	var pages []byte
	for l := 0; l < 128; l++ {
		var pg [128]byte
		zero, full := true, true
		for i := 0; i < 256; i++ {
			n := src[Idx(i/16, y0+l, i%16)] & 15
			if n != 0 {
				zero = false
			}
			if n != 15 {
				full = false
			}
			if i%2 == 0 {
				pg[i/2] |= n
			} else {
				pg[i/2] |= n << 4
			}
		}
		switch {
		case zero:
			idx[l] = 0x80
		case full:
			idx[l] = 0x81
		default:
			idx[l] = byte(len(pages) / 128)
			pages = append(pages, pg[:]...)
		}
	}
	return append(idx, pages...)
}

// unrleAll inflates and run-length expands a payload (tests and tools).
func unrleAll(comp []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	return unrle(b)
}
