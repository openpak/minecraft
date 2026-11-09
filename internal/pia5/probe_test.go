package pia5

import (
	"encoding/hex"
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

// The real Wii U joiner's probe from the 2026-11-07 join (decrypted, joiner RVCID 4) gets a type 1 reply that echoes its token,
// shaped like the real host's reply to the same probe (0000000301282ef4000000007c8d09ea: RVCID 3, type 1, 3 garbage bytes, token).
// The joiner only completes NAT traversal on a type 1 reply; v0.2.6 put a ms timer there, so its replies read as probes.
func TestHostRepliesToRealJoinerProbe(t *testing.T) {
	c, _ := NewCipher(make([]byte, 16), 3)
	var got []Message
	h := NewHost(c, netip.MustParseAddrPort("127.0.0.1:22960"), 2000000001, func(_ netip.AddrPort, _ Header, _ [8]byte, msgs []Message) {
		got = append(got, msgs...)
	})
	probe, _ := hex.DecodeString("0000000400aaa640000000007c8d09ea")
	h.HandleMessages(netip.MustParseAddrPort("10.0.0.5:5212"), Header{}, []Message{{Flags: 9, Proto: ProtoProbe, Port: 1, Payload: probe}})
	var reqs, reps int
	for _, m := range got {
		if m.Proto != ProtoProbe {
			continue
		}
		switch {
		case m.Port == 1 && m.Payload[4] == probeRequest:
			reqs++
		case m.Port == 2 && hex.EncodeToString(m.Payload) == "0000fff001000000000000007c8d09ea":
			reps++
		default:
			t.Fatalf("unexpected probe message port %d payload %x", m.Port, m.Payload)
		}
	}
	if reqs != 2 || reps != 1 {
		t.Fatalf("probes %d replies %d, want 2 and 1", reqs, reps)
	}
}
