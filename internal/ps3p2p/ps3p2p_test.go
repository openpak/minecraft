package ps3p2p

import (
	"bytes"
	"encoding/hex"
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
	if len(got) != 20 || got[0] != 0x80 || got[1] != 0x14 || got[2] != 0 || got[3] != 1 || got[4] != 0x80 || got[5] != GuessWindowByte {
		t.Fatalf("syn frame %x", got)
	}
	if hex.EncodeToString(got[4:]) != "80ff1000"+"0000"+"01000100"+"01020304"+"0546" {
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
	if a.C.PeerMSS() != GuessMSS {
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
