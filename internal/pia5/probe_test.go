package pia5

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
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

// ProbeTo (the joiner asked the NEX server to make the host probe it) probes a known address again and a new one on first sight.
func TestHostProbeTo(t *testing.T) {
	c, _ := NewCipher(make([]byte, 16), 3)
	var probes atomic.Int32
	h := NewHost(c, netip.MustParseAddrPort("127.0.0.1:22960"), 2000000001, func(_ netip.AddrPort, _ Header, _ [8]byte, msgs []Message) {
		for _, m := range msgs {
			if m.Proto == ProtoProbe && m.Port == 1 {
				probes.Add(1)
			}
		}
	})
	a := netip.MustParseAddrPort("10.0.0.3:6000")
	h.ProbeTo(a)
	h.ProbeTo(a)
	if n := probes.Load(); n != 4 {
		t.Fatalf("probes sent: %d, want 4", n)
	}
}

// ProbeTo keeps probing until the joiner is heard from (its NAT drops early probes), then probes once more and stops.
func TestHostProbeToRepeatsUntilHeard(t *testing.T) {
	probeEvery, probeFor = 10*time.Millisecond, time.Second
	defer func() { probeEvery, probeFor = 250*time.Millisecond, 20*time.Second }()
	c, _ := NewCipher(make([]byte, 16), 3)
	var probes atomic.Int32
	h := NewHost(c, netip.MustParseAddrPort("127.0.0.1:22960"), 2000000001, func(_ netip.AddrPort, _ Header, _ [8]byte, msgs []Message) {
		for _, m := range msgs {
			if m.Proto == ProtoProbe && m.Port == 1 {
				probes.Add(1)
			}
		}
	})
	a := netip.MustParseAddrPort("10.0.0.4:7000")
	h.ProbeTo(a)
	time.Sleep(100 * time.Millisecond)
	if n := probes.Load(); n < 8 {
		t.Fatalf("probes before the joiner answered: %d, want repeats", n)
	}
	h.HandleMessages(a, Header{}, nil)
	n := probes.Load()
	time.Sleep(100 * time.Millisecond)
	if m := probes.Load(); m != n {
		t.Fatalf("probes after the joiner answered: %d more, want none", m-n)
	}
}
