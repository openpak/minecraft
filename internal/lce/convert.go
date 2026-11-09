package lce

import (
	"github.com/openpak/minecraft/internal/javamc"
	"github.com/openpak/minecraft/internal/lcechunk"
)

// MaxBlockID is the highest Java block id passed to the console unchanged. The capture shows ids up to 0xFE (254) in chunk
// palettes, but only ids the console world generator itself places were seen; newer 1.8 blocks are shown as air until a capture
// shows what the console does with them (docs/provenance.md, unconfirmed U3).
const MaxBlockID = 174

// Convert turns a Java column into the console's column. Sections the server did not send are air under open sky.
func Convert(j *javamc.ChunkColumn) *lcechunk.Column {
	c := new(lcechunk.Column)
	for i := range c.SkyLight {
		c.SkyLight[i] = 15
	}
	for i := range j.Sections {
		s := &j.Sections[i]
		for y := 0; y < 16; y++ {
			for z := 0; z < 16; z++ {
				for x := 0; x < 16; x++ {
					n := (y*16+z)*16 + x
					v := s.Blocks[n]
					if v>>4 > MaxBlockID {
						v = 0
					}
					o := lcechunk.Idx(x, s.Y*16+y, z)
					c.Block[o] = v
					c.BlockLight[o] = nib(s.BlockLight[:], n)
					c.SkyLight[o] = nib(s.SkyLight[:], n)
				}
			}
		}
	}
	if len(j.Biomes) == 256 {
		copy(c.Biome[:], j.Biomes)
	}
	return c
}

func nib(b []byte, i int) uint8 {
	if i%2 == 0 {
		return b[i/2] & 15
	}
	return b[i/2] >> 4
}
