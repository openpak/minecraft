package pia5

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// capture opens the 2026-11-07 capture's raw packets and session key (MC_CAPTURE_DIR, kept out of the repo: the key is a
// live session secret). Skipped when absent.
func capture(t *testing.T) (c *Cipher, pkts [][]byte, fromHost []bool) {
	dir := os.Getenv("MC_CAPTURE_DIR")
	if dir == "" {
		t.Skip("MC_CAPTURE_DIR not set")
	}
	keys, err := os.ReadFile(filepath.Join(dir, "session.keys"))
	if err != nil {
		t.Skip(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw_packets.bin"))
	if err != nil {
		t.Skip(err)
	}
	for len(raw) >= 3 {
		n := int(binary.BigEndian.Uint16(raw[1:]))
		fromHost = append(fromHost, raw[0] == 0)
		pkts = append(pkts, raw[3:3+n])
		raw = raw[3+n:]
	}
	// the capture's gathering is 2; the key file holds the keys of the capture's sessions, the first is this join's
	for i := 0; i+32 <= len(keys); i += 32 {
		c, _ = NewCipher(keys[i:i+32], 2)
		if _, _, _, err := c.Open(pkts[0]); err == nil {
			return
		}
	}
	t.Fatal("no session key opens the first packet")
	return
}

func TestCaptureOpensAndReseals(t *testing.T) {
	c, pkts, _ := capture(t)
	for i, p := range pkts {
		h, n8, msgs, err := c.Open(p)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if got := c.Seal(h, n8, msgs); !bytes.Equal(got, p) {
			t.Fatalf("packet %d does not re-seal to the same bytes", i)
		}
	}
	t.Logf("%d packets open and re-seal byte for byte", len(pkts))
}
