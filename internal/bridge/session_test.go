package bridge

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/lce/walk"
	"github.com/openpak/minecraft/internal/ps3join"
)

func unhex(s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		panic(err)
	}
	return b
}

// What a real PS3 host sent and received in mc5 (2026-11-13 16:35Z, scratch/ps3-signaling-2026-11-09/mc5.{rx,tx}.bin; the same constants as
// ps3join's game_test): the host's first message (00 KeepAlive + 02 ClientboundPreLogin, one PlayerUID: the host's), its keep-alive and its
// closing FF, and the E8 PreLogin and hello a PS3 joiner "SwitchTest" sent.
const (
	mc5Prelog  = "e80884000a0053007700690074006300680054006500730074"
	mc5Reply   = "00000000000208840001002d000000000001505333546573740000000000000000000080001fa732aa1a00000003000000000000000000000000000000003e9d0000000000000000000f"
	mc5Keep    = "0000000000"
	mc5Dropped = "ff00000003"
)

// rig is a fake Wii U joiner (lce.Client: the packets of the 2026-11-07 capture's joiner) and a fake PS3 host (what its transport writes is
// collected; its answers are the recorded bytes), around one Session.
type rig struct {
	t       *testing.T
	s       *Session
	cl      *lce.Client
	mu      sync.Mutex
	ps3     [][]byte // what the PS3 host received (translated)
	wiiu    [][]byte // what the Wii U received
	left    string
	env     ps3join.GameEnv
	layer   ps3join.GameLayer
	hostPID uint32
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, hostPID: 1630024257}
	r.s = NewSession("ps3:e8ca9e01df86", r.hostPID, 1570020199, "PS3Test", nil, nil, func() {})
	uid := ps3join.PlayerUID("SwitchTest", 0, [6]byte{0x00, 0x1f, 0xa7, 0x0f, 0x00, 0x01}, 1)
	host := ps3join.PlayerUID("PS3Test", 0, [6]byte{0x00, 0x1f, 0xa7, 0x32, 0xaa, 0x1a}, 3)
	r.env = ps3join.GameEnv{Name: "SwitchTest", UID: uid, Host: host, Small: 1,
		Write: func(b []byte) { r.mu.Lock(); r.ps3 = append(r.ps3, bytes.Clone(b)); r.mu.Unlock() },
		Leave: func(why string) { r.mu.Lock(); r.left = why; r.mu.Unlock() }}
	r.layer = r.s.Layer(r.env)
	// the Wii U: its PIA session is attached before the PS3 leg is ready
	r.cl = &lce.Client{Name: "WiiUPlayer", PID: 1570020199, HostPID: r.hostPID, Send: func(m []byte) {}}
	r.cl.Send = func(m []byte) { r.s.OnWiiU(m) }
	handle := r.s.Attach(func(m []byte) {
		r.mu.Lock()
		r.wiiu = append(r.wiiu, bytes.Clone(m))
		r.mu.Unlock()
		r.cl.OnGame(m)
	}, func() {})
	if handle == nil {
		t.Fatal("no handler")
	}
	return r
}

func (r *rig) ps3Got() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.ps3...)
}

func (r *rig) wiiuGot() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.wiiu...)
}

func wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

// The login exchange, byte for byte: the Wii U's FE hello is answered by the leg (FF, the host under its Wii U PID), the Wii U's E8 waits until
// the PS3 leg is ready and then reaches the host as mc5's E8 (net version 0x0884, the joiner's PS3 online id), the host's first message reaches
// the Wii U as a Wii U 02 (0x0881, 20-byte host id), the Wii U's 01 reaches the host with the PS3 identity in both id fields, and the host's
// disconnect reaches the Wii U and ends the session.
func TestLoginExchange(t *testing.T) {
	r := newRig(t)
	r.cl.Start() // FE hello out
	t.Cleanup(r.cl.Stop)
	// ; the leg's FF comes back and the client sends E8
	wait(t, "FF hello", func() bool { return len(r.wiiuGot()) == 1 })
	ff := r.wiiuGot()[0]
	if ff[0] != 0xFF || len(ff) < 137 || binary.BigEndian.Uint32(ff[29:]) != r.hostPID || cstrT(ff[33:57]) != "PS3Test" ||
		binary.BigEndian.Uint32(ff[109:]) != 1570020199 || cstrT(ff[113:137]) != "WiiUPlayer" {
		t.Fatalf("hello: host pid %d name %q, joiner pid %d name %q", binary.BigEndian.Uint32(ff[29:]), cstrT(ff[33:57]), binary.BigEndian.Uint32(ff[109:]), cstrT(ff[113:137]))
	}
	hostID := sha1.Sum([]byte("openpak lce wiiu host 1630024257"))
	if !bytes.Equal(ff[1:21], hostID[:]) {
		t.Fatalf("host id %x", ff[1:21])
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(r.ps3Got()); n != 0 {
		t.Fatalf("%d writes to the host before the PS3 leg was ready", n)
	}
	// the PS3 leg is ready: its hello went out (the transport's), Start releases what the Wii U sent
	r.layer.Start()
	got := r.ps3Got()
	if len(got) != 1 || hex.EncodeToString(got[0]) != mc5Prelog {
		t.Fatalf("PS3 host got %x, want mc5's E8 %s", got, mc5Prelog)
	}
	// the host's first message, as recorded
	r.layer.OnGame(unhex(mc5Reply))
	wait(t, "Wii U 02", func() bool { return len(r.wiiuGot()) == 2 })
	m := r.wiiuGot()[1]
	// the 02 of the PS3 host with the Wii U's net version and the host's 20-byte id in place of its 28-byte PlayerUID
	ps3 := unhex(mc5Reply)
	pk, err := walk.Next(ps3[5:], walk.PS3)
	if err != nil {
		t.Fatal(err)
	}
	o := 5 + pk.UIDs[0]
	want := append([]byte{0, 1}, ps3[:o]...)
	want[2+5+1], want[2+5+2] = 0x08, 0x81
	want = append(append(want, hostID[:]...), ps3[o+28:]...)
	if p, err := walk.Next(m[2+5:], walk.WiiU); err != nil || len(p.UIDs) != 1 || !bytes.Equal(m[2+5+p.UIDs[0]:2+5+p.UIDs[0]+20], hostID[:]) {
		t.Fatalf("Wii U 02: %v uids %v: %x", err, p.UIDs, m)
	}
	if !bytes.Equal(m, want) {
		t.Fatalf("Wii U 02\n got  %x\n want %x", m, want)
	}
	// the Wii U's 01 Login reaches the host as the one joinps3 sends (same walker, same translation), with our PS3 identity in both id fields
	wait(t, "01 at the host", func() bool { return len(r.ps3Got()) >= 2 })
	login := r.ps3Got()[1]
	var sent [][]byte
	g := &ps3join.Game{Name: "SwitchTest", UID: r.env.UID, Write: func(b []byte) { sent = append(sent, bytes.Clone(b)) }}
	g.Start()
	g.OnGame(unhex(mc5Reply))
	if len(sent) != 2 {
		t.Fatalf("reference game wrote %d messages", len(sent))
	}
	p, err := walk.Next(login, walk.PS3)
	if err != nil || p.ID != 0x01 || len(p.Raw) != len(login) || len(p.UIDs) != 2 {
		t.Fatalf("01 at the host: %v %x", err, login)
	}
	for _, o := range p.UIDs {
		if !bytes.Equal(login[o:o+28], r.env.UID[:]) {
			t.Fatalf("PlayerUID in the 01 %x, want ours", login[o:o+28])
		}
	}
	if !bytes.Equal(login, sent[1]) {
		t.Fatalf("01 differs from joinps3's\n bridge %x\n joinps3 %x", login, sent[1])
	}
	// keep-alives pass through; the clock tick (channel 3) never reaches the host
	r.layer.OnGame(unhex(mc5Keep))
	wait(t, "keep-alive at the Wii U", func() bool { return len(r.wiiuGot()) >= 3 })
	for _, w := range r.wiiuGot() {
		if len(w) >= 2 && w[1] == 3 {
			t.Fatalf("a channel 3 tick came from the leg")
		}
	}
	for _, b := range r.ps3Got() {
		if bytes.HasPrefix(b, []byte{0, 3}) {
			t.Fatalf("a channel 3 message reached the host: %x", b)
		}
	}
	// the host's disconnect is passed on and ends the session
	r.layer.OnGame(unhex(mc5Dropped))
	wait(t, "end", r.s.Ended)
	last := r.wiiuGot()[len(r.wiiuGot())-1]
	if !bytes.Equal(last, append([]byte{0, 1}, unhex(mc5Dropped)...)) {
		t.Fatalf("last message to the Wii U %x", last)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !strings.Contains(r.left, "PS3 host disconnected") {
		t.Fatalf("the PS3 leg was told %q", r.left)
	}
}

// The Wii U leaving ends the session and tells the PS3 leg; a session the PS3 side ends gives the Wii U a disconnect of our own.
func TestEnds(t *testing.T) {
	r := newRig(t)
	r.layer.Start()
	r.s.OnWiiU(append([]byte{0, 1}, 0xFF, 0, 0, 0, 1))
	wait(t, "end", r.s.Ended)
	r.mu.Lock()
	if !strings.Contains(r.left, "Wii U disconnected") {
		t.Fatalf("left %q", r.left)
	}
	r.mu.Unlock()
	if got := r.ps3Got(); len(got) != 1 || !bytes.Equal(got[0], unhex("ff00000001")) {
		t.Fatalf("the Wii U's FF was not passed on: %x", got)
	}
	r2 := newRig(t)
	r2.layer.Start()
	r2.s.End("ps3 leg failed")
	got := r2.wiiuGot()
	if len(got) == 0 || !bytes.Equal(got[len(got)-1], append([]byte{0, 1}, unhex("ff00000003")...)) {
		t.Fatalf("Wii U got %x", got)
	}
}

// Game bytes cut anywhere reach the Wii U whole: a chunk packet (0x33) goes on channel 2 in pieces of 5,120 bytes, other packets on channel 1,
// and a packet split across PS3 messages is held until it is whole.
func TestRechunk(t *testing.T) {
	r := newRig(t)
	r.layer.Start()
	chunk := func(n int) []byte {
		b := []byte{0x33, 1, 0, 0, 0, 16, 0, 0, 0, 0, 0, 32, 15, 255, 15}
		b = binary.BigEndian.AppendUint32(b, uint32(n))
		b = append(b, bytes.Repeat([]byte{0xab}, n)...)
		return append(b, 0, 0)
	}
	big := chunk(12000)
	if p, err := walk.Next(big, walk.PS3); err != nil || len(p.Raw) != len(big) {
		t.Fatalf("test chunk does not walk: %v", err)
	}
	// delivered in 1,346-byte messages like the PS3's transport cuts it, then one keep-alive
	stream := append(bytes.Clone(big), unhex(mc5Keep)...)
	for len(stream) > 0 {
		n := min(len(stream), 1346)
		r.layer.OnGame(stream[:n])
		stream = stream[n:]
	}
	var ch2 []byte
	var pieces []int
	var ch1 [][]byte
	for _, m := range r.wiiuGot() {
		switch {
		case len(m) >= 2 && m[0] == 0 && m[1] == 2:
			pieces = append(pieces, len(m)-2)
			ch2 = append(ch2, m[2:]...)
		case len(m) >= 2 && m[0] == 0 && m[1] == 1:
			ch1 = append(ch1, m[2:])
		}
	}
	if !bytes.Equal(ch2, big) || len(pieces) != 3 || pieces[0] != 5120 || pieces[1] != 5120 || pieces[2] != len(big)-2*5120 {
		t.Fatalf("channel 2: %d bytes in pieces %v, want the chunk (%d bytes) in 5120-byte pieces", len(ch2), pieces, len(big))
	}
	if len(ch1) != 1 || !bytes.Equal(ch1[0], unhex(mc5Keep)) {
		t.Fatalf("channel 1 %x", ch1)
	}
}

func TestRename(t *testing.T) {
	e8 := append([]byte{0xE8, 0x08, 0x81}, u16str("WiiUPlayer")...)
	if got := hex.EncodeToString(rename(e8, 3, "SwitchTest")); got != strings.Replace(mc5Prelog, "e80884", "e80881", 1) { // the name only
		t.Fatalf("%s", got)
	}
}

func u16str(s string) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(s)))
	for _, c := range s {
		b = binary.BigEndian.AppendUint16(b, uint16(c))
	}
	return b
}

func cstrT(b []byte) string { return cstr(b) }
