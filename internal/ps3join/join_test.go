package ps3join

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openpak/minecraft/internal/ps3p2p"
	"github.com/openpak/minecraft/internal/rtsig"
)

// A fake PS3 host (a ps3p2p endpoint with its own skey on a loopback socket) against the probe's real UDP path: kind-6 datagrams in both
// directions, the RUDP handshake, the table-before-hello rule, the hello and E8 on the wire, and the host's first game packet recorded.
func TestProbeAgainstFakeHost(t *testing.T) {
	probeKey := bytes.Repeat([]byte{0x11}, 16)
	hostKey := bytes.Repeat([]byte{0x22}, 16)
	hostSock, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	probeSock, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer hostSock.Close()
	defer probeSock.Close()
	probeAddr := probeSock.LocalAddr().(*net.UDPAddr)

	rec, err := NewRecorder(t.TempDir(), "t")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	s := &probe{cfg: Config{Label: "t", OutDir: t.TempDir(), OnlineID: "SwitchTest", PreLogin: true}, rec: rec, udp: probeSock, member: 0x20}
	s.uid = PlayerUID("SwitchTest", 0, [6]byte{0, 0x1f, 0xa7, 0, 0, 1}, 1)
	s.p = &rtsig.Participant{SKey: probeKey, SID: 7, HostJID: "Host@gb.gb.np.playstation.net/ps3", SelfJID: "SwitchTest@gb.gb.np.playstation.net/ps3",
		Send: func(string) error { return nil }}
	if err := s.p.Handle(rtsig.Stanza{Kind: "set", ID: "o", NS: rtsig.NSUdpp2p, Action: "offer", SID: 9, SKey: hostKey,
		From: "Host@gb.gb.np.playstation.net/ps3", To: "SwitchTest@gb.gb.np.playstation.net/ps3",
		Candidates: []rtsig.Candidate{{Type: "local", Address: "127.0.0.1", Port: uint16(hostSock.LocalAddr().(*net.UDPAddr).Port)}}}); err != nil {
		t.Fatal(err)
	}
	go s.readLoop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.ticker(ctx)

	// the fake host
	var hc *ps3p2p.Conn
	var mu sync.Mutex
	var msgs []string
	hc = ps3p2p.NewConn(1, 0x4000, 0x99, func(f []byte) {
		d, _ := ps3p2p.SealRandom(probeKey, f)
		hostSock.WriteToUDP(d, probeAddr)
	})
	hm := &ps3p2p.Msgr{C: hc}
	hm.OnGame = func(k uint16, p []byte) {
		mu.Lock()
		msgs = append(msgs, string(rune('0'+k))+":"+string(p[:1])+":"+itoa(len(p)))
		mu.Unlock()
	}
	hm.Attach()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := hostSock.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if plain, _, err := ps3p2p.Open(hostKey, buf[:n]); err == nil {
				hc.Handle(plain, time.Now())
			}
		}
	}()
	go func() {
		for ctx.Err() == nil {
			hc.Tick(time.Now())
			time.Sleep(50 * time.Millisecond)
		}
	}()

	s.want("test")
	hc.Start(time.Now())
	waitUntil(t, "rudp connected on both sides", func() bool { return hc.Established() && s.snap().RudpConnected && s.hostSeen() })
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	if len(msgs) != 0 {
		t.Fatalf("the probe spoke before the host wrote its table entry: %v", msgs)
	}
	mu.Unlock()

	// the host writes entry 1 for member 0x20 (small id 3) and the probe may speak
	var a57 [128]byte
	copy(a57[:], "Host")
	binary.BigEndian.PutUint16(a57[0x1c:], 0x10)
	binary.BigEndian.PutUint16(a57[32+0x1c:], 0x20)
	a57[32+0x1e] = 3
	s.updateTable(a57[:], nil, "test")
	waitUntil(t, "hello and E8", func() bool { mu.Lock(); defer mu.Unlock(); return len(msgs) == 2 })
	mu.Lock()
	if msgs[0] != "1:\x03:32" || !strings.HasPrefix(msgs[1], "2:\xe8:") {
		t.Errorf("messages %q", msgs)
	}
	mu.Unlock()
	if !s.snap().HelloSent {
		t.Error("HelloSent")
	}
	// the host's first game packet (02 ClientboundPreLogin would be first; any bytes are recorded, the walker may lose sync)
	hm.Write(ps3p2p.KindAcked, []byte{0x00, 0x00}, time.Now()) // 00 KeepAlive with an int
	waitUntil(t, "first game packet", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.prog.FirstGame })
	t.Log(s.snap())
}

func (s *probe) snap() Progress { s.mu.Lock(); defer s.mu.Unlock(); return s.prog }

func (s *probe) hostSeen() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.hostAddr != nil }

func itoa(n int) string { return intStr(n) }

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}
