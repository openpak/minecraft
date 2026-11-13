package ps3join

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/openpak/minecraft/internal/lce/walk"
)

func unhex(s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		panic(err)
	}
	return b
}

// What a real PS3 host sent mc5 (2026-11-13 16:35Z, scratch/ps3-signaling-2026-11-09/mc5.rx.bin, 219 bytes): one 74-byte kind-1 message that is
// a 00 KeepAlive (int 0) followed by the 02 ClientboundPreLogin (net version 0x0884, login key "-", one PlayerUID: the host's), then the same
// 5-byte 00 KeepAlive once a second for 28 s, then at 30 s FF Disconnect reason 3 (the host's 600-tick login timeout).
const (
	mc5Hello   = "01000000 537769746368546573740000000000000080001fa70f000100000001" // our hello, mc5.tx.bin[:32]
	mc5Prelog  = "e80884000a0053007700690074006300680054006500730074"                // our E8 PreLogin, mc5.tx.bin[32:]
	mc5Reply   = "00000000000208840001002d000000000001505333546573740000000000000000000080001fa732aa1a00000003000000000000000000000000000000003e9d0000000000000000000f"
	mc5Keep    = "0000000000"
	mc5Dropped = "ff00000003"
)

func newGame(t *testing.T) (*Game, *[][]byte) {
	t.Helper()
	var sent [][]byte
	uid := PlayerUID("SwitchTest", 0, [6]byte{0x00, 0x1f, 0xa7, 0x0f, 0x00, 0x01}, 1)
	if got := hex.EncodeToString(Hello(1, uid)); got != strings.ReplaceAll(mc5Hello, " ", "") {
		t.Fatalf("our hello is not mc5's: %s", got)
	}
	g := &Game{Name: "SwitchTest", UID: uid, Write: func(b []byte) { sent = append(sent, bytes.Clone(b)) }}
	t.Cleanup(g.Stop)
	return g, &sent
}

// The login after mc5: E8 goes out at once, the host's 74 bytes (the 00 and the 02) make the next thing we send the 01 Login, whole and in the
// PS3 dialect, with our 28-byte PlayerUID in both id fields; the host's keep-alives and its closing FF change nothing we send.
func TestGameLoginAfterMc5(t *testing.T) {
	for _, cut := range []int{0, 30, 73} { // the message as the host sent it, and cut inside the PlayerUID and before the last byte
		g, sent := newGame(t)
		g.Start()
		if len(*sent) != 1 || hex.EncodeToString((*sent)[0]) != mc5Prelog {
			t.Fatalf("after Start: %x (mc5 sent %s)", *sent, mc5Prelog)
		}
		reply := unhex(mc5Reply)
		if len(reply) != 74 {
			t.Fatalf("reply %d bytes", len(reply))
		}
		if cut == 0 {
			g.OnGame(reply)
		} else {
			g.OnGame(reply[:cut])
			g.OnGame(reply[cut:])
		}
		if len(*sent) != 2 {
			t.Fatalf("cut %d: %d writes after the host's 02, want the E8 and the 01: %x", cut, len(*sent), *sent)
		}
		login := (*sent)[1]
		if cut == 0 {
			t.Logf("our 01 Login (%d bytes): %x", len(login), login)
		}
		p, err := walk.Next(login, walk.PS3)
		if err != nil || len(p.Raw) != len(login) || p.ID != 0x01 {
			t.Fatalf("our 01 does not walk as one whole PS3 packet: %v (%d of %d bytes)", err, len(p.Raw), len(login))
		}
		if !bytes.HasPrefix(login, unhex("01 0000013c 000a 00530077006900740063006800540065007300740000")) { // client version 316, name, "" second string
			t.Fatalf("login head %x", login[:30])
		}
		if len(p.UIDs) != 2 || !bytes.Equal(login[p.UIDs[0]:p.UIDs[0]+28], g.UID[:]) || !bytes.Equal(login[p.UIDs[1]:p.UIDs[1]+28], g.UID[:]) {
			t.Fatalf("PlayerUIDs in the login: %v, want ours twice", p.UIDs)
		}
		if got := login[p.UIDs[1]+28 : p.UIDs[1]+33]; !bytes.Equal(got, make([]byte, 5)) { // the host's 02 had byte 0, int 0
			t.Fatalf("echo of the host's UGC pair: %x", got)
		}
		// keep-alives for 28 s and the FF at 30 s: nothing more is sent, the walker keeps going, the reason is recorded
		for i := 0; i < 28; i++ {
			g.OnGame(unhex(mc5Keep))
		}
		g.OnGame(unhex(mc5Dropped))
		st := g.Stats()
		if len(*sent) != 2 || g.Lost() != nil || !st.PreLogin || st.Login || st.Packets != 2+28+1 || st.Disconnect != "0x3" {
			t.Fatalf("cut %d: sent %d lost %v stats %+v", cut, len(*sent), g.Lost(), st)
		}
	}
}

// The host answers a login whose UGC int differs from the one in its 02 with the 02 again (login+0x88 != netmgr+0xe4, EB 0xbc1a24): the
// byte and int of the 02 come back in our 01.
func TestGameEchoesUGCPair(t *testing.T) {
	g, sent := newGame(t)
	g.Start()
	reply := unhex(mc5Reply)
	reply[5+1+2+4] = 1                               // friends-only byte, after the 5-byte keep-alive, id, version, the one-character login key
	binary.BigEndian.PutUint32(reply[5+1+2+4+1:], 7) // UGC players version
	g.OnGame(reply)
	login := (*sent)[1]
	p, _ := walk.Next(login, walk.PS3)
	if got := hex.EncodeToString(login[p.UIDs[1]+28 : p.UIDs[1]+33]); got != "0100000007" {
		t.Fatalf("echo %s", got)
	}
}

// From the login response on it is the Wii U joiner's behaviour: the 01 response and the host's D6 (the player's position) are answered
// with 87 (accept teleport) and 0D (position), translated for the PS3 (no PlayerUID in either), and chunks (33) are counted.
func TestGameInWorld(t *testing.T) {
	g, sent := newGame(t)
	g.Start()
	g.OnGame(unhex(mc5Reply))
	resp := append(unhex("01 000000ed 0000 0000"), make([]byte, 23+28+28+44)...) // entity 237, two empty strings, the two PlayerUIDs as all-zero
	if p, err := walk.Next(resp, walk.PS3); err != nil || len(p.Raw) != len(resp) {
		t.Fatalf("test 01 response: %v", err)
	}
	d6 := []byte{0xd6}
	for _, v := range []float64{-141.5, 73, 258.5} {
		d6 = binary.BigEndian.AppendUint64(d6, math.Float64bits(v))
	}
	d6 = append(d6, 0x00, 0x05) // no look, teleport id 5
	g.OnGame(append(resp, d6...))
	st := g.Stats()
	if !st.Login || !st.InWorld || st.EntityID != 237 || st.X != -141.5 || st.Y != 73 || st.Z != 258.5 {
		t.Fatalf("stats %+v", st)
	}
	if len(*sent) != 3 {
		t.Fatalf("%d writes, want E8, 01, 87+0D", len(*sent))
	}
	want := append(append([]byte{0x87, 0x05, 0x0d}, d6[1:25]...), make([]byte, 8+1)...) // look 0, ground 0
	if !bytes.Equal((*sent)[2], want) {
		t.Fatalf("answer %x, want %x", (*sent)[2], want)
	}
	if p, _, err := walk.Walk((*sent)[2], walk.PS3); err != nil || len(p) != 2 {
		t.Fatalf("our answer does not walk: %v", err)
	}
}

// An id no layout covers ends the walking, not the session: the rest is left for the raw recording and nothing panics or is answered.
func TestGameUnknownPacketDoesNotAbort(t *testing.T) {
	g, sent := newGame(t)
	g.Start()
	g.OnGame(append(unhex(mc5Reply), 0x15, 1, 2, 3)) // 15 is no packet id
	if g.Lost() == nil || len(*sent) != 2 {
		t.Fatalf("lost %v, %d writes", g.Lost(), len(*sent))
	}
	g.OnGame(unhex(mc5Keep))
	if st := g.Stats(); !st.PreLogin || st.Packets != 2 {
		t.Fatalf("%+v", st)
	}
}
