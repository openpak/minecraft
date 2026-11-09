package pia5

import (
	"bytes"
	"testing"
)

func TestSealOpen(t *testing.T) {
	c, err := NewCipher(bytes.Repeat([]byte{7}, 32), 0x123456)
	if err != nil {
		t.Fatal(err)
	}
	in := []Message{{Flags: 1, Dest: 2, Src: 0x5d949b50, Proto: ProtoReliable, Port: 0, Payload: []byte("hello")}, {Flags: 9, Proto: ProtoProbe, Port: 1, Payload: make([]byte, 16)}}
	pkt := c.Seal(Header{Conn: 0xbb, ID: 3, Timer: 10, Echo: 11}, [8]byte{0, 3, 0, 0x39, 1, 2, 3, 4}, in)
	if (len(pkt)-36)%16 != 0 {
		t.Fatalf("ciphertext not whole blocks: %d", len(pkt))
	}
	h, n, out, err := c.Open(pkt)
	if err != nil || h.Conn != 0xbb || h.ID != 3 || n[7] != 4 || len(out) != 2 || string(out[0].Payload) != "hello" || out[1].Port != 1 || out[0].Src != 0x5d949b50 {
		t.Fatalf("open: %v %+v %v %+v", err, h, n, out)
	}
	pkt[40] ^= 1
	if _, _, _, err := c.Open(pkt); err == nil {
		t.Fatal("tampered packet accepted")
	}
}
