package javamc

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"testing"
)

func TestVarInt(t *testing.T) {
	for _, v := range []int{0, 1, 127, 128, 300, 25565, 1 << 21, -1} {
		got, n, err := ReadVarInt(AppendVarInt(nil, v))
		if err != nil || got != v || n == 0 {
			t.Fatalf("%d -> %d %v", v, got, err)
		}
	}
}

func TestColumns(t *testing.T) {
	// one section (index 4) in a ground-up column with sky light
	var d []byte
	for i := 0; i < 4096; i++ {
		d = binary.LittleEndian.AppendUint16(d, uint16(i%256)<<4|3)
	}
	d = append(d, bytes.Repeat([]byte{0x12}, 2048)...)
	d = append(d, bytes.Repeat([]byte{0xff}, 2048)...)
	d = append(d, bytes.Repeat([]byte{7}, 256)...)
	p := binary.BigEndian.AppendUint32(nil, 0xfffffffe)
	p = binary.BigEndian.AppendUint32(p, 5)
	p = append(p, 1)
	p = binary.BigEndian.AppendUint16(p, 1<<4)
	p = AppendVarInt(p, len(d))
	p = append(p, d...)
	cols, err := Columns(ChunkData, p, true)
	if err != nil || len(cols) != 1 {
		t.Fatal(err)
	}
	c := cols[0]
	if c.X != -2 || c.Z != 5 || len(c.Sections) != 1 || c.Sections[0].Y != 4 || c.Sections[0].Blocks[257] != uint16(1)<<4|3 || c.Sections[0].SkyLight[9] != 0xff || c.Biomes[3] != 7 {
		t.Fatalf("bad column %+v", c)
	}
}

// A fake server: handshake, login, compression on, one Keep Alive.
func TestLoginAndCompression(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		s := &Client{conn: conn, r: newBuf(conn), threshold: -1}
		s.Read() // handshake
		s.Read() // login start
		s.Write(0x03, AppendVarInt(nil, 16))
		s.threshold = 16
		s.Write(0x02, append(AppendString(AppendString(nil, "uuid-1"), "Bot"), 0))
		s.Write(KeepAlive, AppendVarInt(nil, 77))
		s.Read()
	}()
	c, err := Dial(ln.Addr().String(), "Bot")
	if err != nil {
		t.Fatal(err)
	}
	id, data, err := c.Read()
	if err != nil || id != KeepAlive {
		t.Fatalf("%d %v", id, err)
	}
	if v, _, _ := ReadVarInt(data); v != 77 {
		t.Fatalf("token %d", v)
	}
	c.KeepAlive(77)
}

// TestLiveServer joins a real 1.8.9 server (MC_JAVA_ADDR) and decodes what it sends until a few chunks have arrived.
func TestLiveServer(t *testing.T) {
	addr := os.Getenv("MC_JAVA_ADDR")
	if addr == "" {
		t.Skip("MC_JAVA_ADDR not set")
	}
	c, err := Dial(addr, "Probe")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var pos, chunks int
	for chunks < 20 {
		id, data, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		switch id {
		case KeepAlive:
			v, _, _ := ReadVarInt(data)
			c.KeepAlive(v)
		case JoinGame:
			c.Settings(4)
		case PosAndLook:
			r := NewReader(data)
			t.Logf("position %.1f %.1f %.1f", r.Double(), r.Double(), r.Double())
			pos++
		case ChunkData, ChunkBulk:
			cols, err := Columns(id, data, true)
			if err != nil {
				t.Fatalf("columns: %v", err)
			}
			chunks += len(cols)
		}
	}
	if pos == 0 {
		t.Fatal("no position packet")
	}
	t.Logf("%d chunks decoded", chunks)
}
