package pia5

import (
	"net/netip"
	"testing"
)

// A console host sends two NAT probe requests as soon as a joiner appears (capture: the first two host packets); a joiner waits
// for them before it connects.
func TestHostProbesNewJoiner(t *testing.T) {
	c, _ := NewCipher(make([]byte, 16), 3)
	var probes int
	h := NewHost(c, netip.MustParseAddrPort("127.0.0.1:22960"), 2000000001, func(_ netip.AddrPort, _ Header, _ [8]byte, msgs []Message) {
		for _, m := range msgs {
			if m.Proto == ProtoProbe && m.Port == 1 && m.Flags == 9 && len(m.Payload) == 16 {
				probes++
			}
		}
	})
	from := netip.MustParseAddrPort("10.0.0.2:5000")
	h.HandleMessages(from, Header{}, nil)
	h.HandleMessages(from, Header{}, nil) // the same address again: no more probes
	if probes != 2 {
		t.Fatalf("probes sent: %d, want 2", probes)
	}
}
