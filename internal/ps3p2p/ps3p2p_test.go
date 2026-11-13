package ps3p2p

import (
	"bytes"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"
)

// Vectors computed independently of this package: AES-128 through the openssl CLI (CBC for the full blocks, ECB of the previous
// ciphertext block or the IV for the tail), HMAC-SHA1 through Python, from the rules of spec §1.3-1.4.
var vectors = []struct{ name, seed, plain, dgram string }{
	{"tail only (shorter than a block)", "01020304", "01020304050607", "ffc60101010203042bef00753ac2d91604a7bc"},
	{"exactly one block", "deadbeef", "000102030405060708090a0b0c0d0e0f", "ffc60101deadbeef825c454af229076bea33e0f59ca449bdbb1c8df4"},
	{"two blocks and a 5-byte tail", "0badf00d", "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021222324",
		"ffc601010badf00dbde160cfb23e70e61ce5fca14f213aa2d53d542c1d4f944e2612939ccab583a14848294b704cbaebb6"},
}

var testKey, _ = hex.DecodeString("00112233445566778899aabbccddeeff")

func TestSealOpenVectors(t *testing.T) {
	for _, v := range vectors {
		plain, _ := hex.DecodeString(v.plain)
		want, _ := hex.DecodeString(v.dgram)
		seed := uint32(0)
		for _, b := range mustHex(v.seed) {
			seed = seed<<8 | uint32(b)
		}
		got, err := Seal(testKey, seed, plain)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: Seal = %x, %v; want %x", v.name, got, err, want)
		}
		back, s, err := Open(testKey, want)
		if err != nil || s != seed || !bytes.Equal(back, plain) {
			t.Errorf("%s: Open = %x seed %x, %v", v.name, back, s, err)
		}
	}
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

func TestOpenRejects(t *testing.T) {
	d := mustHex(vectors[1].dgram)
	bad := append([]byte(nil), d...)
	bad[len(bad)-1] ^= 1
	if _, _, err := Open(testKey, bad); err != ErrMAC {
		t.Errorf("flipped ciphertext: %v", err)
	}
	other := mustHex("ffeeddccbbaa99887766554433221100")
	if _, _, err := Open(other, d); err != ErrMAC {
		t.Errorf("wrong key: %v", err)
	}
	// the signalling datagrams share the socket: kind 3 (ff 83 ...) is not ours
	if _, _, err := Open(testKey, append([]byte{0xff, 0x83, 0xff, 0xfe, 0xff, 0xfe}, make([]byte, 20)...)); err != ErrNotKind6 {
		t.Errorf("kind 3: %v", err)
	}
}

// The first packet of the Terraria golden test, as a class-2 frame: header 0x8000 | 4 + 14, then the port.
func TestSynFrame(t *testing.T) {
	var got []byte
	c := NewConn(1, 0x1000, 0x01020304, func(d []byte) { got = d })
	c.Start(time.Now())
	if len(got) != 20 || got[0] != 0x80 || got[1] != 0x14 || got[2] != 0 || got[3] != 1 || got[4] != 0x80 || got[5] != WindowByte {
		t.Fatalf("syn frame %x", got)
	}
	if hex.EncodeToString(got[4:]) != "80301000"+"0201"+"01000100"+"01020304"+"0582" {
		t.Errorf("syn packet %x", got[4:])
	}
}

type link struct {
	mu   sync.Mutex
	drop func(n int) bool
	n    int
}

// pair joins two endpoints through the real crypto: each seals with the other's skey and opens with its own.
func pair(t *testing.T, drop func(n int) bool) (a, b *Msgr, tick func(time.Time)) {
	ka, kb := mustHex("000102030405060708090a0b0c0d0e0f"), mustHex("f0e0d0c0b0a090807060504030201000")
	var ca, cb *Conn
	l := &link{drop: drop}
	send := func(to **Conn, sealKey, openKey []byte) func([]byte) {
		q := make(chan []byte, 1000) // one ordered path per direction, as a LAN is
		go func() {
			for d := range q {
				plain, _, err := Open(openKey, d)
				if err != nil {
					t.Errorf("open: %v", err)
					continue
				}
				(*to).Handle(plain, time.Now())
			}
		}()
		return func(frame []byte) {
			d, err := SealRandom(sealKey, frame)
			if err != nil {
				t.Error(err)
				return
			}
			l.mu.Lock()
			l.n++
			dropped := l.drop != nil && l.drop(l.n)
			l.mu.Unlock()
			if dropped {
				return
			}
			q <- d
		}
	}
	ca = NewConn(1, 0x0100, 0xaaaa0001, send(&cb, kb, kb)) // a -> b: encrypted with b's skey, b opens with it
	cb = NewConn(1, 0x7000, 0xbbbb0002, send(&ca, ka, ka))
	a, b = &Msgr{C: ca}, &Msgr{C: cb}
	a.Attach()
	b.Attach()
	ca.Start(time.Now())
	cb.Start(time.Now())
	return a, b, func(now time.Time) { ca.Tick(now); cb.Tick(now) }
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestLoopbackMessages(t *testing.T) {
	a, b, _ := pair(t, nil)
	waitFor(t, "both established", func() bool { return a.C.Established() && b.C.Established() })
	if a.C.PeerMSS() != MSS {
		t.Errorf("peer MSS %d", a.C.PeerMSS())
	}
	var mu sync.Mutex
	var got []struct {
		kind uint16
		p    []byte
	}
	b.OnGame = func(k uint16, p []byte) {
		mu.Lock()
		got = append(got, struct {
			kind uint16
			p    []byte
		}{k, p})
		mu.Unlock()
	}
	hello := bytes.Repeat([]byte{7}, 32)
	if err := a.Write(KindPlain, hello, time.Now()); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 2*MaxPiece+10) // three pieces: 1346, 1346, 10
	for i := range big {
		big[i] = byte(i)
	}
	if err := a.WriteGame(big, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "four messages", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 4 })
	if got[0].kind != KindPlain || !bytes.Equal(got[0].p, hello) {
		t.Errorf("hello: %v %x", got[0].kind, got[0].p)
	}
	var joined []byte
	for i, g := range got[1:] {
		if g.kind != KindAcked || len(g.p) > MaxPiece || (i < 2 && len(g.p) != MaxPiece) {
			t.Errorf("piece %d: kind %d len %d", i, g.kind, len(g.p))
		}
		joined = append(joined, g.p...)
	}
	if !bytes.Equal(joined, big) {
		t.Error("game bytes differ")
	}
	waitFor(t, "acks drain the in-flight counter", func() bool { return a.Inflight() == 0 })
}

func TestLoopbackRetransmit(t *testing.T) {
	// drop the first data-carrying datagram a sends after the handshake
	dropped := false
	var armed bool
	var am sync.Mutex
	var a, b *Msgr
	var tick func(time.Time)
	a, b, tick = pair(t, func(n int) bool {
		am.Lock()
		defer am.Unlock()
		if armed && !dropped {
			dropped = true
			return true
		}
		return false
	})
	waitFor(t, "established", func() bool { return a.C.Established() && b.C.Established() })
	var mu sync.Mutex
	n := 0
	b.OnGame = func(uint16, []byte) { mu.Lock(); n++; mu.Unlock() }
	am.Lock()
	armed = true
	am.Unlock()
	a.Write(KindPlain, []byte("one"), time.Now())
	a.Write(KindPlain, []byte("two"), time.Now())
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if n != 0 {
		t.Fatalf("delivered %d out of order", n)
	}
	mu.Unlock()
	tick(time.Now().Add(GuessRTO + time.Millisecond))
	waitFor(t, "both after the retransmit, in order", func() bool { mu.Lock(); defer mu.Unlock(); return n == 2 })
}

// The two datagrams a real PS3 (Terraria v01.12) sent on run live10, 2026-11-13, without the UDPP2P header (`ff c3 01 01`), which here is
// the kind-6 layer's business: its SYN and its reset. The Minecraft leg runs the same librudp, so the same replies are proven for it.
func TestReplayRealConsoleSynAndReset(t *testing.T) {
	var out [][]byte
	c := NewConn(1, 0x1000, 0xaaaa0001, func(d []byte) { out = append(out, append([]byte(nil), d...)) })
	c.Start(time.Now())
	out = nil
	var lines []string
	c.Trace = func(dir, desc string, _ []byte) { lines = append(lines, dir+" "+desc) }
	c.Handle(mustHex("80140001"+"80303d4c"+"0201"+"0100"+"0100"+"f8ba92b1"+"0582"), time.Now())
	want := "80160001" + "a0301000" + "3d4d" + "0201" + "0100" + "0100" + "aaaa0001" + "0582"
	if len(out) != 1 || hex.EncodeToString(out[0]) != want {
		t.Fatalf("reply to the console's SYN = %x, want %s", out, want)
	}
	if c.PeerMSS() != 0x582 || c.Established() {
		t.Errorf("peer mss %d established %v", c.PeerMSS(), c.Established())
	}
	if !strings.Contains(strings.Join(lines, "\n"), "rx SYN flags=0x00 win=0x30 seq=0x3d4c cap=0x0201 verA=0x0100 verB=0x0100 connid=0xf8ba92b1 mss=1410") {
		t.Errorf("trace:\n%s", strings.Join(lines, "\n"))
	}
	// its handshake-completing data packet with the ack flag (rule from the librudp decode, state 3)
	c.Handle(mustHex("800a0001"+"20303d4d"+"1001"), time.Now())
	if !c.Established() {
		t.Error("not established by the console's ack")
	}
	// its reset: recorded, never answered
	reset := false
	c.OnReset = func() { reset = true }
	n := len(out)
	c.Handle(mustHex("800e0001"+"c0000000"+"0003"+"f8ba92b1"), time.Now())
	if !reset || len(out) != n {
		t.Errorf("reset seen %v, answered %d datagrams", reset, len(out)-n)
	}
}

// The SYN retransmit timer is librudp's: 1 s, doubling, capped at 16 s.
func TestSynBackoff(t *testing.T) {
	var out [][]byte
	c := NewConn(1, 0x1000, 1, func(d []byte) { out = append(out, d) })
	t0 := time.Unix(100, 0)
	c.Start(t0)
	out = nil
	now := t0
	for _, step := range []time.Duration{1, 2, 4, 8, 16, 16} {
		c.Tick(now.Add(step*time.Second - time.Millisecond))
		if len(out) != 0 {
			t.Fatalf("early retransmit before %v s", step)
		}
		now = now.Add(step * time.Second)
		c.Tick(now)
		if len(out) != 1 {
			t.Fatalf("no retransmit at +%v s", step)
		}
		out = nil
	}
}

// established is a Conn in the data phase whose datagrams are collected (as parsed packets) instead of sent.
func established(out *[][]byte) *Conn {
	c := NewConn(1, 0x100, 0xabcd, func(f []byte) { *out = append(*out, f[4:]) }) // aggregator header + port
	c.state, c.started, c.havePeer, c.expect = stEstab, true, true, 0x7000
	return c
}

// A message longer than one segment goes out as segments of at most MSS-0x40 = 1346 bytes, all but the last flagged "more" (the PS3's own
// 1,350-byte messages arrive as 1,346 + 4 with flags 0x02 and 0x00).
func TestSendFragments(t *testing.T) {
	var out [][]byte
	c := established(&out)
	msg := make([]byte, 4+MaxPiece)
	if err := c.Send(msg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("%d datagrams", len(out))
	}
	_, f0, _, _, _, off0, _ := parse(out[0])
	_, f1, _, _, _, off1, _ := parse(out[1])
	if f0 != flagAck|flagMore || f1 != flagAck || len(out[0])-off0 != 1346 || len(out[1])-off1 != 4 {
		t.Errorf("flags %#x %#x, sizes %d %d", f0, f1, len(out[0])-off0, len(out[1])-off1)
	}
}

// The receive side takes messages from the byte stream: a message cut over segments (and a header cut in two) comes out whole and in order.
func TestMsgrReassembly(t *testing.T) {
	var stream []byte
	var want [][]byte
	for i, n := range []int{1346, 5, 1346, 0, 300, 1346} {
		p := bytes.Repeat([]byte{byte(i + 1)}, n)
		kind := uint16(KindPlain)
		if i%2 == 1 {
			kind = KindAcked
		}
		stream = append(stream, byte(n>>8), byte(n), 0, byte(kind))
		stream = append(stream, p...)
		if n > 0 {
			want = append(want, p)
		}
	}
	for _, cut := range []int{1346, 1, 3, 7} {
		var got [][]byte
		m := &Msgr{C: NewConn(1, 1, 1, func([]byte) {})}
		m.OnGame = func(k uint16, p []byte) { got = append(got, p) }
		for s := stream; len(s) > 0; s = s[min(cut, len(s)):] {
			m.recv(s[:min(cut, len(s))])
		}
		if len(got) != len(want) {
			t.Fatalf("cut %d: %d messages, want %d", cut, len(got), len(want))
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("cut %d: message %d differs", cut, i)
			}
		}
	}
}

// A silent peer is not flooded: the resend interval doubles up to GuessRTOMax, and after GuessDeadAfter without an ack the connection is
// declared dead once and takes no more data.
func TestRetransmitBackoffAndDead(t *testing.T) {
	var out [][]byte
	c := established(&out)
	dead := make(chan struct{}, 4)
	c.OnDead = func() { dead <- struct{}{} }
	t0 := time.Now()
	c.Send([]byte("0123456789"), t0)
	for i := 1; i <= 100*40; i++ { // 40 s in 10 ms steps
		c.Tick(t0.Add(time.Duration(i) * 10 * time.Millisecond))
	}
	// first send, resends at 0.5, 1.5, 3.5, 7.5, 15.5, 23.5 s, dead at 30 s (keepalives are empty acks, the same packet class)
	data := 0
	for _, p := range out {
		if _, _, _, _, _, off, err := parse(p); err == nil && off < len(p) {
			data++
		}
	}
	if data != 7 {
		t.Errorf("%d data datagrams in 30 s, want 7", data)
	}
	select {
	case <-dead:
	case <-time.After(time.Second):
		t.Fatal("not declared dead")
	}
	select {
	case <-dead:
		t.Error("declared dead twice")
	case <-time.After(50 * time.Millisecond):
	}
	if err := c.Send([]byte("x"), t0); err == nil {
		t.Error("a dead connection took data")
	}
}
