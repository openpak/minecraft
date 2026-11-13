package ps3join

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/lce/walk"
)

// Game is the game layer of a PS3 joiner, the counterpart of pia5.Joiner + lce.Client on the Wii U leg (internal/relay/console.go): the
// transport (RUDP messages, the 32-byte hello) is the caller's; Game takes the host's game bytes in through OnGame and hands ours out through
// Write. It does not write a second client: the Wii U joiner lce.Client runs the login (E8, 02 -> 01, 01 response, D6 -> 87 + 0D, keep-alive
// and position every second), and Game translates both directions with internal/lce/walk (net version 0x0881 <-> 0x0884, PlayerUID 20 <->
// 28 bytes), splits the PS3 stream (no channel prefix, a plain packet stream) into the Wii U channel messages lce.Client reads, and
// puts the PS3 specifics on the 01 login (transport note §9): the joiner's own PlayerUID in both id fields and the host's friends-only/UGC
// pair echoed from its 02.
//
// Order on the wire (mc5 2026-11-13 and the host's handlers, EB 0xbc1760 / 0xbc17ec / 0xbc1a24): joiner hello (kind 1, 32 bytes), E8 PreLogin
// -> the host answers 00 KeepAlive + 02 PreLogin (one message) -> joiner 01 Login -> host accepts (01 response, world sync, D6, chunks).
// A host that gets no 01 for 600 ticks (30 s) sends FF reason 3 and drops the connection; mc5 was that.
type Game struct {
	Name string
	UID  [28]byte // the joiner's PlayerUID, as in the hello and the host's player table
	// Write sends game bytes (whole packets, any number) to the host.
	Write func(b []byte)
	// Logf, if set, records free-text lines (the recorder's Logf has this form).
	Logf func(tag, format string, a ...any)
	// OnPacket, if set, sees every packet of the host ("rx") and every one we send ("tx"), in the PS3 dialect.
	OnPacket func(dir string, p walk.Packet)

	mu   sync.Mutex
	cl   *lce.Client
	uids walk.UIDMap
	buf  []byte // host bytes of a packet not yet whole
	lost error  // the walker could not go on; the raw stream is still recorded by the caller
	ugc  []byte // the host's last 02: friends-only byte and UGC players version int, echoed in our 01
}

func (g *Game) logf(tag, format string, a ...any) {
	if g.Logf != nil {
		g.Logf(tag, format, a...)
	}
}

// Start runs the login: lce.Client sends E8 at once and then follows the host. Call it right after the hello.
func (g *Game) Start() {
	cl := &lce.Client{Send: g.fromClient, Name: g.Name, NoSession: true, Log: slog.New(slog.NewTextHandler(logWriter{g}, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))}
	g.mu.Lock()
	g.cl = cl
	g.mu.Unlock()
	cl.Start()
}

// Stop ends the keep-alive loop.
func (g *Game) Stop() {
	g.mu.Lock()
	cl := g.cl
	g.mu.Unlock()
	if cl != nil {
		cl.Stop()
	}
}

// Stats is what the host has sent so far (lce.Client's counters: pre-login reply, login response, in world, packets, chunks, position,
// the host's disconnect reason).
func (g *Game) Stats() lce.ClientStats {
	g.mu.Lock()
	cl := g.cl
	g.mu.Unlock()
	if cl == nil {
		return lce.ClientStats{}
	}
	return cl.Stats()
}

// Lost is why the walker stopped, nil while it has not. The session itself goes on; nothing is answered past that point.
func (g *Game) Lost() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lost
}

// OnGame takes the payload of one kind 1 or kind 2 message of the host. Messages may cut a packet anywhere (pieces of 1,346 bytes).
func (g *Game) OnGame(b []byte) {
	g.mu.Lock()
	if g.lost != nil {
		g.mu.Unlock()
		return
	}
	pkts, rest, err := walk.Walk(append(g.buf, b...), walk.PS3)
	g.buf = bytes.Clone(rest)
	if err != nil {
		g.lost = err
	}
	cl := g.cl
	g.mu.Unlock()
	var ch1 []byte
	for _, p := range pkts {
		g.logf("WALK", "rx %#02x %s, %d bytes", p.ID, walk.Name(p.ID), len(p.Raw))
		if g.OnPacket != nil {
			g.OnPacket("rx", p)
		}
		if p.ID == 0x02 {
			g.noteUGC(p)
		}
		w := walk.Translate(p, walk.PS3, walk.WiiU, &g.uids)
		if p.ID == 0x33 { // lce.Client counts chunks on its channel 2
			g.toClient(cl, append([]byte{0, 2}, w...))
			continue
		}
		ch1 = append(ch1, w...)
	}
	if len(ch1) > 0 {
		g.toClient(cl, append([]byte{0, 1}, ch1...))
	}
	if err != nil {
		g.logf("WALK", "host stream: %v; nothing past this point is answered, the raw bytes are in the .rx.bin", err)
	}
}

func (g *Game) toClient(cl *lce.Client, msg []byte) {
	if cl != nil {
		cl.OnGame(msg)
	}
}

// noteUGC keeps the byte and int after the id list's header in the host's 02: ver short, UTF, byte, int (walk.clientPreLogin).
func (g *Game) noteUGC(p walk.Packet) {
	i := p.Version + 2
	n := int(int16(binary.BigEndian.Uint16(p.Raw[i:])))
	i += 2
	if n > 0 {
		i += 2 * n
	}
	g.mu.Lock()
	g.ugc = bytes.Clone(p.Raw[i : i+5])
	g.mu.Unlock()
}

// fromClient takes what lce.Client sends (Wii U channel messages): channel 1 packets are translated and written to the host; the FE hello
// and channel-3 ticks have no PS3 counterpart (Client.NoSession leaves them out) and anything else is dropped.
func (g *Game) fromClient(msg []byte) {
	if len(msg) < 2 || msg[0] != 0 || msg[1] != 1 || g.Write == nil {
		return
	}
	pkts, _, err := walk.Walk(msg[2:], walk.WiiU)
	if err != nil {
		g.logf("LCE", "our own packet does not walk: %v", err) // a bug, not the host's doing
	}
	var out []byte
	for _, p := range pkts {
		b := walk.Translate(p, walk.WiiU, walk.PS3, &g.uids)
		if p.ID == 0x01 {
			b = g.login(bytes.Clone(b))
		}
		if q, err := walk.Next(b, walk.PS3); err == nil {
			g.logf("WALK", "tx %#02x %s, %d bytes", q.ID, walk.Name(q.ID), len(b))
			if g.OnPacket != nil {
				g.OnPacket("tx", q)
			}
		}
		out = append(out, b...)
	}
	if len(out) > 0 {
		g.Write(out)
	}
}

// login makes the translated 01 a PS3 joiner's: both PlayerUID fields are ours (the host takes the first one unless it is empty, else the
// second, for the new player, and refuses a second login of a UID already in the game with FF reason 0x19: EB 0xbc1a24, 0xb81e10), and the
// byte and int after them are the host's own from its 02 (the host compares the int with the value it put in the 02 and answers a
// mismatch with the 02 again instead of accepting: EB 0xbc1a24 `login+0x88 == netmgr+0xe4`, else 0xbc0edc).
func (g *Game) login(b []byte) []byte {
	q, err := walk.Next(b, walk.PS3)
	if err != nil || len(q.UIDs) != 2 {
		return b
	}
	for _, o := range q.UIDs {
		copy(b[o:], g.UID[:])
	}
	g.mu.Lock()
	ugc := g.ugc
	g.mu.Unlock()
	if len(ugc) == 5 {
		copy(b[q.UIDs[1]+len(g.UID):], ugc)
	}
	return b
}

// logWriter lets lce.Client's slog lines land in the recorder.
type logWriter struct{ g *Game }

func (w logWriter) Write(p []byte) (int, error) {
	w.g.logf("LCE", "%s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// StatsString is the one-line account of the game layer for the summary.
func StatsString(st lce.ClientStats, lost error) string {
	y := func(b bool) string {
		if b {
			return "yes"
		}
		return "NO"
	}
	d := st.Disconnect
	if d == "" {
		d = "-"
	}
	s := fmt.Sprintf("prelogin_reply=%s login_response=%s in_world=%s packets=%d chunks=%d pos=(%.1f,%.1f,%.1f) entity=%d host_disconnect=%s",
		y(st.PreLogin), y(st.Login), y(st.InWorld), st.Packets, st.Chunks, st.X, st.Y, st.Z, st.EntityID, d)
	if lost != nil {
		s += " walker_lost=" + lost.Error()
	}
	return s
}
