package ps3join

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The host's own table as the real PS3 wrote it (room log 2026-11-11, entry 0: PS3Test, flags 0x80, member 0x10, small id 0, one entry).
func TestTableAndHello(t *testing.T) {
	host := PlayerUID("PS3Test", 0, [6]byte{0, 0x1f, 0xa7, 1, 2, 3}, 3)
	var a57 [128]byte
	copy(a57[:], host[:])
	binary.BigEndian.PutUint16(a57[0x1c:], 0x10)
	a57[0x1f] = 0x01 // pad 0, two entries once we are in
	// our entry: no UID yet, member 0x20, small id 1, pad 0
	binary.BigEndian.PutUint16(a57[32+0x1c:], 0x20)
	a57[32+0x1e] = 1
	es := ParseTable(a57[:], nil)
	if len(es) != 2 || es[0].Name() != "PS3Test" || es[0].Count != 1 {
		t.Fatalf("entries %+v", es)
	}
	e, ok := Find(es, 0x20)
	if !ok || e.Small != 1 || e.Pad != 0 {
		t.Fatalf("our entry %+v", e)
	}
	if _, ok := Find(es, 0x10); ok {
		t.Error("the host's own entry is not a joiner entry")
	}
	uid := PlayerUID("SwitchTest", 0, [6]byte{0, 0x1f, 0xa7, 9, 9, 9}, 1)
	h := Hello(e.Small, uid)
	if len(h) != 32 || h[0] != 1 || !bytes.Equal(h[4:14], []byte("SwitchTest")) || h[4+0x11] != 0x80 || h[1]|h[2]|h[3] != 0 {
		t.Fatalf("hello %x", h)
	}
}
