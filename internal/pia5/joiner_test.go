package pia5

import (
	"encoding/hex"
	"net"
	"net/netip"
	"testing"
	"time"
)

// sink is a PacketConn that opens what the joiner writes.
type sink struct {
	c    *Cipher
	to   []netip.AddrPort
	msgs [][]Message
}

func (s *sink) WriteTo(b []byte, a net.Addr) (int, error) {
	_, _, m, err := s.c.Open(b)
	if err != nil {
		panic(err)
	}
	s.to = append(s.to, a.(*net.UDPAddr).AddrPort())
	s.msgs = append(s.msgs, m)
	return len(b), nil
}
func (s *sink) ReadFrom([]byte) (int, net.Addr, error) { select {} }
func (s *sink) Close() error                           { return nil }
func (s *sink) LocalAddr() net.Addr                    { return &net.UDPAddr{} }
func (s *sink) SetDeadline(time.Time) error            { return nil }
func (s *sink) SetReadDeadline(time.Time) error        { return nil }
func (s *sink) SetWriteDeadline(time.Time) error       { return nil }

// take returns the messages written since the last call.
func (s *sink) take() []Message {
	var out []Message
	for _, m := range s.msgs {
		out = append(out, m...)
	}
	s.msgs = nil
	return out
}

func hx(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// TestJoinerAgainstRecordedHost feeds the real console host's messages from the 2026-11-07 join (decrypted,
// pia_plain_host_to_joiner.bin, the host Wii U 10.87.0.8 as PID 0x5d949b50) to a Joiner that has the recorded joiner's identity
// (PID 0x6327d978, CID 0x7b51c8ed, RVCID 4, public 10.89.2.24:56989, private 10.87.0.2:5316, ack id 0x7cacb3a5), and checks
// each answer against the bytes the recorded joiner sent (pia_plain_joiner_to_host.bin), in order: probe exchange, connection
// request, connection response, session join, acks, RTT answer.
func TestJoinerAgainstRecordedHost(t *testing.T) {
	c, _ := NewCipher(make([]byte, 16), 2)
	out := &sink{c: c}
	host := netip.MustParseAddrPort("10.87.0.8:54432")
	j := &Joiner{Conn: out, Cipher: c, PID: 0x6327d978, HostPID: 0x5d949b50, CID: 0x7b51c8ed, RVCID: 4,
		Self: netip.MustParseAddrPort("10.89.2.24:56989"), Private: netip.MustParseAddrPort("10.87.0.2:5316"), Targets: []netip.AddrPort{host}}
	j.init()
	j.ourAck = 0x7cacb3a5
	feed := func(proto, port uint8, p string) []Message {
		j.handle(host, Header{}, []Message{{Flags: 1, Src: 0x5d949b50, Proto: proto, Port: port, Payload: hx(p)}})
		return out.take()
	}
	expect := func(step string, got []Message, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d messages, want %d", step, len(got), len(want))
		}
		for i := range want {
			if g := hex.EncodeToString(got[i].Payload); g != want[i] {
				t.Fatalf("%s message %d:\n got %s\nwant %s", step, i, g, want[i])
			}
		}
	}

	// the joiner probes its target twice (type 0, its RVCID) and nothing else until a reply comes back
	j.kick()
	probes := out.take()
	if len(probes) != 2 || probes[0].Proto != ProtoProbe || probes[0].Port != 1 || probes[0].Flags != 9 || hex.EncodeToString(probes[0].Payload[:5]) != "0000000400" {
		t.Fatalf("first probes: %+v", probes)
	}
	// 12:39:52.0046 host probe -> .0180 joiner type 1 reply echoing the token (bytes 5-7 are never set by the game: the capture
	// has 0ef24f there, we send zeros)
	expect("probe reply", feed(ProtoProbe, 1, "0000000300fac8400000007d817ce4c1"), "00000004010000000000007d817ce4c1")
	// .2973 host type 1 reply -> .3029 connection request, byte for byte
	expect("connection request", feed(ProtoProbe, 2, "0000000301282ef4000000007c8d09ea"),
		"01c40800000000005d949b5000000000000a590218de9d000000006327d9787b51c8ed00000004010f00010203000000000000000a57000214c4000000006327d9787b51c8ed00000004010f00010200000000000000007cacb3a5")
	if j.Host != host {
		t.Fatalf("host %v", j.Host)
	}
	expect("host ack", feed(ProtoStation, 0, "050000007cacb3a5"))
	// .3453 host inverse request -> .3520 ack and connection response
	resp := "0200080300000000005d949b501e09ec21467269656e64" + zeros(29) + "02010101" + zeros(122) + "ff" + zeros(72) + "7cacb3a6"
	expect("connection response", feed(ProtoStation, 0, "01bb0801000000006327d9787b51c8edc40a590218c1f2000000005d949b501e09ec2100000003010f00010203000000000000000a57000814a2000000005d949b501e09ec2100000003010f000102000000000000000059cdf587"),
		"0500000059cdf587", resp)
	// .3973 host connection response -> .4161 ack, .4357 session join
	expect("session join", feed(ProtoStation, 0, "0200080300000000006327d9787b51c8ed467269656e64"+zeros(29)+"02010101"+zeros(122)+"ff"+zeros(72)+"59cdf588"),
		"0500000059cdf588", "01fd00000a590218de9d7cacb3a7")
	// .4535 join response (destination 0) -> .4652 ack
	expect("join ack", feed(ProtoSession, 0, "020200010100020008000800000000020a590218c1f2000000005d949b501e09ec2100000003010f00010203000000000000000a57000814a2000000005d949b501e09ec2100000003010f000102000000000000000000000a590218de9d000000006327d9787b51c8ed00000004010f00010203000000000000000a57000214c4000000006327d9787b51c8ed00000004010f0001020000000000000000010059cdf589"),
		"0500000059cdf589")
	// .4535 host RTT request -> .4652 the answer, to the host station (destination 1) from our PID
	got := feed(ProtoRTT, 0, "00000000000000000000007d83205320")
	expect("rtt answer", got, "00000001000000000000007d83205320")
	if got[0].Dest != 1 || got[0].Src != 0x6327d978 {
		t.Fatalf("rtt answer dest %d src %x", got[0].Dest, got[0].Src)
	}
	// connected: the keepalives start with an RTT request and a clock request, both to the host station
	j.mu.Lock()
	j.keepalives()
	j.mu.Unlock()
	ka := out.take()
	if len(ka) != 2 || ka[0].Proto != ProtoRTT || ka[0].Payload[3] != 0 || ka[1].Proto != ProtoClock || len(ka[1].Payload) != 16 {
		t.Fatalf("keepalives: %+v", ka)
	}
	// the mesh update (588 bytes on 18/1) is acked and opens Joined
	mesh := Reliable{Flags: 3, Seq: FirstSeq, Ack: FirstSeq, Payload: make([]byte, 588)}.Marshal()
	j.handle(host, Header{}, []Message{{Flags: 1, Proto: ProtoSession, Port: 1, Payload: mesh}})
	if a := out.take(); len(a) != 1 || hex.EncodeToString(a[0].Payload) != "000000000000000000000000fffff8300000000000000000" {
		t.Fatalf("mesh ack: %+v", a)
	}
	select {
	case <-j.Joined:
	default:
		t.Fatal("Joined not closed")
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	return hex.EncodeToString(b)
}
