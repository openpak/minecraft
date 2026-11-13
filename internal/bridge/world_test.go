package bridge

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/openpak/minecraft/internal/lce/walk"
	"github.com/openpak/minecraft/internal/lcechunk"
	"github.com/openpak/minecraft/internal/ps3p2p"
)

// testdata/ps3-world-1113.msgs.bin is the message stream a real PS3 (Minecraft 01.84) host sent the bridge on 2026-11-13 17:31 after the
// Wii U's login was accepted, rebuilt from the RUDP segments of the session trace (the trace's own .rx.bin lost 4 bytes at every RUDP
// fragment boundary, which is the bug this fixture pins), cut after the last whole packet: kind 1, 2 and 3 messages with their 4-byte
// headers, 3,135 packets, 97 chunks.
func worldMessages(t *testing.T) []ps3p2p.Rx {
	t.Helper()
	b, err := os.ReadFile("testdata/ps3-world-1113.msgs.bin")
	if err != nil {
		t.Fatal(err)
	}
	msgs, rest, err := ps3p2p.Split(b)
	if err != nil || len(rest) != 0 {
		t.Fatalf("split: %d bytes left, %v", len(rest), err)
	}
	return msgs
}

func worldGame(t *testing.T) []byte {
	var game []byte
	for _, m := range worldMessages(t) {
		if m.Kind == ps3p2p.KindPlain || m.Kind == ps3p2p.KindAcked {
			game = append(game, m.P...)
		}
	}
	return game
}

// TestWorldStreamWalks: the whole stream splits into named packets with nothing left over, and every PS3 chunk converts to a column the
// Wii U's reader (lcechunk) parses.
func TestWorldStreamWalks(t *testing.T) {
	game := worldGame(t)
	pkts, rest, err := walk.Walk(game, walk.PS3)
	if err != nil || len(rest) != 0 {
		t.Fatalf("walk stopped %d bytes before the end: %v", len(rest), err)
	}
	chunks := 0
	for i, p := range pkts {
		if walk.Name(p.ID) == "unknown" {
			t.Fatalf("packet %d: id %#02x has no name", i, p.ID)
		}
		if p.ID != 0x33 {
			continue
		}
		out := walk.Translate(p, walk.PS3, walk.WiiU, &walk.UIDMap{})
		if bytes.Equal(out, p.Raw) {
			t.Fatalf("chunk %d was not converted", chunks)
		}
		z := out[19 : 19+int(binary.BigEndian.Uint32(out[15:])&0x3fffffff)]
		zr, err := zlib.NewReader(bytes.NewReader(z))
		if err != nil {
			t.Fatalf("chunk %d: %v", chunks, err)
		}
		got, err := io.ReadAll(zr) // also checks the zlib checksum
		if err != nil || len(got) != int(binary.BigEndian.Uint32(p.Raw[19:])) {
			t.Fatalf("chunk %d: %d bytes, %v", chunks, len(got), err)
		}
		// lcechunk (written from Wii U chunks only) reads the column, except tile modes 9 and 15 that no Wii U capture had
		if _, err := lcechunk.Unpack(z); err != nil && !strings.Contains(err.Error(), "unsupported tile mode") {
			t.Fatalf("chunk %d: %v", chunks, err)
		}
		if again, _, err := walk.Walk(out, walk.WiiU); err != nil || len(again) != 1 || len(again[0].Raw) != len(out) {
			t.Fatalf("chunk %d does not walk as one Wii U packet: %v", chunks, err)
		}
		chunks++
	}
	if len(pkts) != 3135 || chunks != 97 {
		t.Errorf("%d packets, %d chunks", len(pkts), chunks)
	}
}

// TestWorldRelay pushes the host's messages through the session as the PS3 leg delivers them (one message at a time, a game message
// cut anywhere): nothing is held back beyond one unfinished packet, chunks go on channel 2 and everything else on channel 1 in order,
// pieces stay within the Wii U's 5,120 bytes, and the two channels carry exactly the translation of the walked stream.
func TestWorldRelay(t *testing.T) {
	var got [][]byte
	s := NewSession("ps3:test", 1, 2, "PS3Test", nil, nil, func() {})
	s.Attach(func(m []byte) { got = append(got, append([]byte(nil), m...)) }, func() {})
	game := worldGame(t)
	for _, m := range worldMessages(t) {
		if m.Kind != ps3p2p.KindPlain && m.Kind != ps3p2p.KindAcked {
			continue
		}
		s.OnPS3(m.P)
		if pk, _, _ := walk.Walk(s.pbuf, walk.PS3); len(pk) != 0 {
			t.Fatalf("%d whole packets held back after a message", len(pk))
		}
	}
	if s.Ended() {
		t.Fatal("session ended")
	}
	if len(s.pbuf) != 0 {
		t.Fatalf("%d bytes held at the end", len(s.pbuf))
	}
	var ch [4][]byte
	for _, m := range got {
		if len(m) < 2 || m[0] != 0 || len(m) > 2+wiiuPiece {
			t.Fatalf("bad message to the Wii U: %d bytes, head %x", len(m), m[:min(len(m), 4)])
		}
		ch[m[1]] = append(ch[m[1]], m[2:]...)
	}
	var want1, want2 []byte
	pkts, _, _ := walk.Walk(game, walk.PS3)
	for _, p := range pkts {
		tr := walk.Translate(p, walk.PS3, walk.WiiU, &s.uids) // the session's id pairs are made on the first pass
		if p.ID == 0x33 {
			want2 = append(want2, tr...)
		} else {
			want1 = append(want1, tr...)
		}
	}
	if !bytes.Equal(ch[1], want1) || !bytes.Equal(ch[2], want2) || len(ch[3]) != 0 {
		t.Errorf("channels differ: ch1 %d/%d bytes, ch2 %d/%d bytes", len(ch[1]), len(want1), len(ch[2]), len(want2))
	}
	// the Wii U's reader must be able to walk both channels as Wii U streams
	for c, b := range [][]byte{ch[1], ch[2]} {
		if _, rest, err := walk.Walk(b, walk.WiiU); err != nil || len(rest) != 0 {
			t.Errorf("channel %d does not walk as Wii U: %d bytes left, %v", c+1, len(rest), err)
		}
	}
}
