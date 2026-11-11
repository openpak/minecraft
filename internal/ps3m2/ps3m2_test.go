package ps3m2

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// bytes laid out as np-matching2's wire package writes them (RoomData.Element, MemberOf, SearchRoomReply): this pins the parsers to that writer.
func internalAttr(id, last uint16, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, last)
	b = append(b, make([]byte, 8)...)
	return TLV(id, 0, append(b, TLV(0x01, 2, data)...))
}

func TestRoomAttrsAndMembers(t *testing.T) {
	t57, t58 := bytes.Repeat([]byte{0x57}, 128), bytes.Repeat([]byte{0x58}, 128)
	room := append(make([]byte, 0x20), TLV(0x41, 7, make([]byte, 32))...)
	room = append(room, internalAttr(0x57, 0x10, t57)...)
	room = append(room, internalAttr(0x58, 0x10, t58)...)
	a, b := RoomAttrs(room, 0x20)
	if !bytes.Equal(a, t57) || !bytes.Equal(b, t58) {
		t.Fatal("room attrs")
	}
	// an event body has no head
	a, _ = RoomAttrs(internalAttr(0x57, 0x20, t57), 0)
	if !bytes.Equal(a, t57) {
		t.Fatal("event attrs")
	}
	m := binary.BigEndian.AppendUint16(nil, 0x10)
	m = append(m, make([]byte, 8)...)
	m = binary.BigEndian.AppendUint32(m, 0x80000000)
	m = append(m, 2, 0)
	m = append(m, TLV(0x0b, 0, NpID("PS3Test", "pt"))...)
	m = append(m, TLV(0x59, 0, append(make([]byte, 8), TLV(0x01, 2, []byte{0, 0, 0, 1})...))...)
	list := append([]byte{0x00, 0x4b, 0, 1}, TLV(0x4b, 0, m)...)
	ms := ParseMembers(list)
	if len(ms) != 1 || ms[0].ID != 0x10 || !ms[0].Owner || ms[0].Name != "PS3Test" || !bytes.Equal(ms[0].Bin59, []byte{0, 0, 0, 1}) {
		t.Fatalf("members %+v", ms)
	}
}

func TestSearchAndFrameMAC(t *testing.T) {
	item := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0, 0, 0, 0, 0, 8}
	item = append(item, NpID("PS3Test", "pt")...)
	item = append(item, TLV(0x41, 7, make([]byte, 32))...)
	el := append([]byte{0x00, 0x45, 0, 1, 0, 0, 0, 1}, TLV(0x45, 0, item)...)
	rs := ParseSearch(el)
	if len(rs) != 1 || rs[0].ID != [3]uint16{0x1234, 0x5678, 0x9abc} || rs[0].Owner != "PS3Test" {
		t.Fatalf("search %+v", rs)
	}
	key := bytes.Repeat([]byte{9}, 16)
	req := Request(key, CmdJoinRoom, [4]byte{1, 2, 3, 4}, TLV(0x3e, 4, []byte{0x9a, 0xbc}))
	f, rest, err := ParseFrame(req)
	if err != nil || len(rest) != 0 || !f.Verify(key) || f.Verify(key[1:]) || f.Elements()[0x3e][1] != 0xbc {
		t.Fatalf("frame %v", err)
	}
}
