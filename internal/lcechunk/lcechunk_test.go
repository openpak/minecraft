package lcechunk

import (
	"os"
	"path/filepath"
	"testing"
)

// The fixtures are chunk payloads cut from the 2026-11-07 Wii U capture (docs/provenance.md).
func captured(t *testing.T) [][]byte {
	files, _ := filepath.Glob("testdata/cap*.zlib")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestCaptureDecodes(t *testing.T) {
	for i, b := range captured(t) {
		c, err := Unpack(b)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		// terrain sanity: the bottom layer of every column is bedrock (id 7), nothing above y 200 but air
		for x := 0; x < W; x++ {
			for z := 0; z < W; z++ {
				if c.Block[Idx(x, 0, z)]>>4 != 7 {
					t.Fatalf("chunk %d: (%d,0,%d) = %#x, want bedrock", i, x, z, c.Block[Idx(x, 0, z)])
				}
				if c.Block[Idx(x, 255, z)] != 0 || c.SkyLight[Idx(x, 255, z)] != 15 {
					t.Fatalf("chunk %d: top of column not open sky", i)
				}
			}
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for i, b := range captured(t) {
		c, err := Unpack(b)
		if err != nil {
			t.Fatal(err)
		}
		d, err := Unpack(Pack(c))
		if err != nil {
			t.Fatalf("chunk %d repack: %v", i, err)
		}
		if *c != *d {
			t.Fatalf("chunk %d: repacked column differs", i)
		}
	}
}

func TestRLE(t *testing.T) {
	in := []byte{0xFF, 1, 1, 1, 1, 1, 0xFF, 0xFF, 0xFF, 0xFF, 2, 0xFF, 0xFF}
	out, err := unrle(rle(in))
	if err != nil || string(out) != string(in) {
		t.Fatalf("rle round trip: %v %x", err, out)
	}
}
