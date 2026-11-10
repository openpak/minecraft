package walk

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func h(s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		panic(err)
	}
	return b
}

// All 130 ids of the Wii U v688 table (getPacketId symbols): the 129 shared with PS3 01.84 plus A2.
func TestCoverage(t *testing.T) {
	if len(layouts) != 130 {
		t.Fatalf("%d layouts, want 130", len(layouts))
	}
}

// E8 from the 11-07 capture's joiner: version 0x0881, name "tobagin" (UTF-16).
func TestPreLoginVersion(t *testing.T) {
	w := h("e8 0881 0007 0074006f0062006100670069006e")
	p, err := Next(w, WiiU)
	if err != nil || len(p.Raw) != len(w) || p.Version != 1 {
		t.Fatalf("%v %d %d", err, len(p.Raw), p.Version)
	}
	got := Translate(p, WiiU, PS3, &UIDMap{})
	if want := h("e8 0884 0007 0074006f0062006100670069006e"); !bytes.Equal(got, want) {
		t.Fatalf("% x", got)
	}
}

// 02 with one host id: the version and the id change, nothing else.
func TestClientPreLoginUID(t *testing.T) {
	id := bytes.Repeat([]byte{0x31}, 20)
	tail := h("0000000000000000000000000000 00000001 02 00000003 01 00000004")
	w := append(append(h("02 0881 0001 0041 05 00000007 01"), id...), tail...)
	p, err := Next(w, WiiU)
	if err != nil || len(p.Raw) != len(w) || len(p.UIDs) != 1 {
		t.Fatalf("%v %d %v", err, len(p.Raw), p.UIDs)
	}
	var m UIDMap
	ps := Translate(p, WiiU, PS3, &m)
	want := append(append(h("02 0884 0001 0041 05 00000007 01"), append(id, make([]byte, 8)...)...), tail...)
	if !bytes.Equal(ps, want) {
		t.Fatalf("% x", ps)
	}
	q, _ := Next(ps, PS3)
	if back := Translate(q, PS3, WiiU, &m); !bytes.Equal(back, w) {
		t.Fatalf("round trip % x", back)
	}
}

// A PS3-born id gets a stable 20-byte stand-in (its SHA-1) and comes back as itself.
func TestUIDMapPS3Origin(t *testing.T) {
	var m UIDMap
	p := []byte("tobagin_ps3_online_id_8bytes")[:28]
	w := m.Map(p, PS3, WiiU)
	if len(w) != 20 || !bytes.Equal(m.Map(p, PS3, WiiU), w) || !bytes.Equal(m.Map(w, WiiU, PS3), p) {
		t.Fatalf("% x", w)
	}
	if got := m.Map(bytes.Repeat([]byte{0xff}, 20), WiiU, PS3); !bytes.Equal(got, bytes.Repeat([]byte{0xff}, 28)) {
		t.Fatalf("fill % x", got)
	}
}

// E7 is a bare PlayerUID: 21 bytes on Wii U, 29 on PS3.
func TestTeleportToEntity(t *testing.T) {
	w := append([]byte{0xe7}, bytes.Repeat([]byte{7}, 20)...)
	if p, err := Next(w, WiiU); err != nil || len(p.Raw) != 21 {
		t.Fatal(err)
	}
	if _, err := Next(w, PS3); !errors.Is(err, ErrShort) {
		t.Fatalf("PS3 reads 28 bytes, got %v", err)
	}
}

// D5 event 2 (VarInt player, int killer, utf "hi") is read the same by both builds and is not rewritten.
func TestCombatEvent2Unchanged(t *testing.T) {
	b := h("d5 00000002 05 00000009 0002 00680069")
	p, err := Next(b, WiiU)
	if err != nil || len(p.Raw) != len(b) {
		t.Fatal(err)
	}
	if got := Translate(p, WiiU, PS3, &UIDMap{}); !bytes.Equal(got, b) {
		t.Fatalf("% x", got)
	}
}

// Stream behaviour: whole packets, then the partial tail; an unknown id stops the walk.
func TestWalkTail(t *testing.T) {
	s := h("00 00000000 e2 05 0b 4060") // keep alive, serverbound keep alive, half a position
	pkts, rest, err := Walk(s, WiiU)
	if err != nil || len(pkts) != 2 || !bytes.Equal(rest, h("0b 4060")) {
		t.Fatalf("%v %d % x", err, len(pkts), rest)
	}
	if _, _, err := Walk(h("e2 05 15 00"), WiiU); !errors.Is(err, ErrUnknown) {
		t.Fatalf("15 is no packet: %v", err)
	}
}

// 28 SetEntityData with one byte entry and one optional block pos entry, closed by 0xFF.
func TestEntityData(t *testing.T) {
	b := h("28 81 01 00000000 00 00 20 0c 09 01 0000000000000001 ff")
	if p, err := Next(b, WiiU); err != nil || len(p.Raw) != len(b) {
		t.Fatalf("%v", err)
	}
}
