package lce

import (
	"os"
	"testing"

	"github.com/openpak/minecraft/internal/javamc"
	"github.com/openpak/minecraft/internal/lcechunk"
)

func TestConvert(t *testing.T) {
	s := javamc.Section{Y: 3}
	s.Blocks[(5*16+7)*16+2] = 1<<4 | 3 // x 2, y 5, z 7
	s.Blocks[0] = 200 << 4             // too new, shown as air
	s.SkyLight[10] = 0xf1
	j := &javamc.ChunkColumn{X: 1, Z: 2, GroundUp: true, Sections: []javamc.Section{s}, Biomes: make([]byte, 256)}
	c := Convert(j)
	if c.Block[lcechunk.Idx(2, 3*16+5, 7)] != 1<<4|3 || c.Block[lcechunk.Idx(0, 3*16, 0)] != 0 {
		t.Fatal("block mapping")
	}
	if c.SkyLight[lcechunk.Idx(4, 3*16, 1)] != 1 || c.SkyLight[lcechunk.Idx(5, 3*16, 1)] != 15 || c.SkyLight[lcechunk.Idx(0, 200, 0)] != 15 {
		t.Fatal("light mapping")
	}
	if _, err := lcechunk.Unpack(lcechunk.Pack(c)); err != nil {
		t.Fatal(err)
	}
}

// TestLiveChunks converts real Java server chunks (MC_JAVA_ADDR) and checks the console codec returns them unchanged.
func TestLiveChunks(t *testing.T) {
	addr := os.Getenv("MC_JAVA_ADDR")
	if addr == "" {
		t.Skip("MC_JAVA_ADDR not set")
	}
	c, err := javamc.Dial(addr, "ConvProbe")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n, size := 0, 0
	for n < 30 {
		id, data, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		switch id {
		case javamc.KeepAlive:
			v, _, _ := javamc.ReadVarInt(data)
			c.KeepAlive(v)
		case javamc.JoinGame:
			c.Settings(4)
		case javamc.ChunkData, javamc.ChunkBulk:
			cols, err := javamc.Columns(id, data, true)
			if err != nil {
				t.Fatal(err)
			}
			for _, j := range cols {
				if len(j.Sections) == 0 {
					continue
				}
				lc := Convert(j)
				z := lcechunk.Pack(lc)
				back, err := lcechunk.Unpack(z)
				if err != nil || *back != *lc {
					t.Fatalf("chunk %d,%d: round trip failed: %v", j.X, j.Z, err)
				}
				n++
				size += len(z)
			}
		}
	}
	t.Logf("%d Java chunks converted, average %d bytes packed", n, size/n)
}
