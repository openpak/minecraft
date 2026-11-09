package pia5

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type rec struct {
	t    float64
	hd   Header
	msgs []Message
}

func plainRecords(t *testing.T, fn string) []rec {
	dir := os.Getenv("MC_CAPTURE_DIR")
	if dir == "" {
		t.Skip("MC_CAPTURE_DIR not set")
	}
	b, err := os.ReadFile(filepath.Join(dir, fn))
	if err != nil {
		t.Skip(err)
	}
	var out []rec
	for o := 0; o < len(b); {
		tm := float64frombits(binary.BigEndian.Uint64(b[o:]))
		conn, id := b[o+8], binary.BigEndian.Uint16(b[o+9:])
		n := int(binary.BigEndian.Uint32(b[o+11:]))
		pl := b[o+15 : o+15+n]
		o += 15 + n
		var msgs []Message
		for q := 0; q+24 <= len(pl) && pl[q] != 0xFF; {
			size := int(binary.BigEndian.Uint16(pl[q+1:]))
			msgs = append(msgs, Message{Flags: pl[q], Dest: binary.BigEndian.Uint64(pl[q+3:]), Src: binary.BigEndian.Uint64(pl[q+11:]), Proto: pl[q+19], Port: pl[q+20], Payload: pl[q+24 : q+24+size]})
			q += (24 + size + 3) &^ 3
		}
		out = append(out, rec{tm, Header{Conn: conn, ID: id}, msgs})
	}
	return out
}

func float64frombits(u uint64) float64 { return math.Float64frombits(u) }

// TestReplayHandshake feeds the capture's joiner packets to the host in order and prints what the host answers beside what the
// console host answered (shape: protocol, port, length, first bytes).
func TestReplayHandshake(t *testing.T) {
	j := plainRecords(t, "pia_plain_joiner_to_host.bin")
	hrec := plainRecords(t, "pia_plain_host_to_joiner.bin")
	cipher, _ := NewCipher(make([]byte, 16), 2)
	var got []string
	out := func(to netip.AddrPort, hd Header, nonce [8]byte, msgs []Message) {
		for _, m := range msgs {
			got = append(got, fmt.Sprintf("conn=%02x id=%d %02x/%d len=%d %x", hd.Conn, hd.ID, m.Proto, m.Port, len(m.Payload), head(m.Payload, 20)))
		}
	}
	h := NewHost(cipher, netip.MustParseAddrPort("10.87.0.2:22960"), 0x5d949b50, out)
	from := netip.MustParseAddrPort("10.87.0.8:50000")
	limit := 24
	for i, r := range j {
		if i >= limit {
			break
		}
		h.HandleMessages(from, r.hd, r.msgs)
		time.Sleep(time.Millisecond)
	}
	t.Log("--- ours")
	for _, g := range got {
		t.Log(g)
	}
	t.Log("--- captured host")
	n := 0
	for _, r := range hrec {
		for _, m := range r.msgs {
			t.Logf("conn=%02x id=%d %02x/%d len=%d %x", r.hd.Conn, r.hd.ID, m.Proto, m.Port, len(m.Payload), head(m.Payload, 20))
		}
		if n++; n > 24 {
			break
		}
	}
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

var _ = time.Second
