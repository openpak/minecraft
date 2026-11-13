// Package bridge relays one console's game session to a game another console hosts (card openpak/prds#177). The first direction: a Wii U
// joins a game a real PS3 hosts. The OpenPak service is neither player nor host. Toward the Wii U it answers on the Wii U transport as the
// station the game's NEX gathering names (internal/relay: PIA 5, the gathering's session key); toward the PS3 it joins the real room as a
// PS3 joiner under the JOINING player's own PS3 identity (internal/ps3join: Matching2, signalling, RUDP). Between them the console's own game
// stream is walked packet by packet and translated (internal/lce/walk: net version, PlayerUID length), re-chunked to each transport's
// message size. What has no counterpart on the other side is made by the leg: the Wii U FE/FF session hello, the PS3 32-byte hello.
//
// A Session is the transport-independent middle: OnWiiU takes the console's reliable messages, OnPS3 the host's game bytes, and two send
// functions put the translations out. The opposite direction (a PS3 into a Wii U-hosted game) reuses it with the dialects swapped.
package bridge

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/lce/walk"
	"github.com/openpak/minecraft/internal/ps3join"
)

// wiiuPiece is the game bytes per reliable message toward the Wii U (internal/lce pieceMax).
const wiiuPiece = 5120

// disconnectReason is the reason of the FF we send when the session ends and the host did not say why: 3 is the one a real PS3 host sent
// (its pending connection timed out, mc5); no other reason is known for either build. A guess for the Wii U.
const disconnectReason = 3

// Session is one bridged console player.
type Session struct {
	Game               string
	HostPID, JoinerPID uint32
	HostName           string // the PS3 host's name as the Wii U shows it
	Trace              *Trace
	Log                *slog.Logger

	mu         sync.Mutex
	state      string
	send       func([]byte) // one reliable message to the Wii U; nil until its PIA peer is attached
	drop       func()
	early      [][]byte // messages for the Wii U that came before it was attached
	ready      bool     // the PS3 leg is joined: its hello left and the game layer exists
	ps3        func([]byte)
	ps3UID     [28]byte
	hostUID    [28]byte
	name       string // the joiner's online id on the PS3 network
	hostID     [20]byte
	joinerUID  [20]byte
	haveJoiner bool
	ugc        []byte
	ended      bool
	hostSaidFF bool
	lastRx     time.Time
	cancel     func()
	leave      func(string)

	wmu  sync.Mutex // Wii U -> PS3 direction: the walker's buffer, the queue, the order of writes
	wbuf []byte
	wq   [][]byte   // Wii U channel-1 bytes that came before the PS3 leg was ready
	pmu  sync.Mutex // PS3 -> Wii U direction
	pbuf []byte
	uids walk.UIDMap
}

// NewSession makes a session for a Wii U joiner of a game; cancel ends the PS3 leg (leaving the room).
func NewSession(game string, hostPID, joinerPID uint32, hostName string, tr *Trace, log *slog.Logger, cancel func()) *Session {
	if log == nil {
		log = slog.Default()
	}
	s := &Session{Game: game, HostPID: hostPID, JoinerPID: joinerPID, HostName: hostName, Trace: tr, cancel: cancel, lastRx: time.Now(), state: "created",
		Log: log.With("game", game, "joiner_pid", joinerPID)}
	h := sha1.Sum([]byte("openpak lce wiiu host " + strconv.FormatUint(uint64(hostPID), 10)))
	copy(s.hostID[:], h[:])
	return s
}

// State is the current state name.
func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Ended reports whether the session is over.
func (s *Session) Ended() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// SetState records a state change: one log line and one trace line per change.
func (s *Session) SetState(state, detail string) {
	s.mu.Lock()
	if s.ended || s.state == state {
		s.mu.Unlock()
		return
	}
	s.state = state
	s.mu.Unlock()
	s.Log.Info("bridge session", "state", state, "detail", detail)
	s.Trace.Logf("STATE", "%s %s", state, detail)
}

// Attach connects the Wii U's PIA peer: send puts one reliable message to the console, drop closes the peer. It returns the handler of the
// console's messages.
func (s *Session) Attach(send func([]byte), drop func()) func([]byte) {
	s.mu.Lock()
	s.send, s.drop = send, drop
	s.lastRx = time.Now()
	ended := s.ended
	s.mu.Unlock()
	s.SetState("wiiu-connected", "the Wii U's PIA session is up")
	s.pmu.Lock()
	for _, m := range s.takeEarly() {
		s.wiiuSend(m)
	}
	s.pmu.Unlock()
	if ended {
		s.finishWiiU()
	}
	return s.OnWiiU
}

func (s *Session) takeEarly() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.early
	s.early = nil
	return e
}

// wiiuSend puts a message on the Wii U leg (queued until the console is attached). Caller holds pmu.
func (s *Session) wiiuSend(msg []byte) {
	s.mu.Lock()
	send := s.send
	if send == nil {
		if len(s.early) < 4096 {
			s.early = append(s.early, bytes.Clone(msg))
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.Trace.WiiU("tx", msg)
	send(msg)
}

// wiiuChannel puts game bytes on a channel, cut into the Wii U's 5,120-byte pieces (the real host does the same, lce.Session.send).
func (s *Session) wiiuChannel(ch byte, data []byte) {
	for first := true; len(data) > 0 || first; first = false {
		n := min(len(data), wiiuPiece)
		s.wiiuSend(append([]byte{0, ch}, data[:n]...))
		data = data[n:]
	}
}

// OnWiiU takes one reliable message of the console: the FE session hello is answered here, channel 1 is the game's packets (translated and
// written to the PS3 host), channel 3 is the console's clock tick (no counterpart, dropped).
func (s *Session) OnWiiU(msg []byte) {
	s.mu.Lock()
	s.lastRx = time.Now()
	s.mu.Unlock()
	s.Trace.WiiU("rx", msg)
	switch {
	case len(msg) > 0 && msg[0] == 0xFE:
		s.hello(msg)
	case len(msg) >= 2 && msg[0] == 0 && msg[1] == 1:
		s.fromWiiU(msg[2:])
	case len(msg) >= 2 && msg[0] == 0 && msg[1] == 3:
	default:
		s.Trace.Logf("WIIU", "message dropped: %d bytes, first %x", len(msg), msg[:min(len(msg), 8)])
	}
}

// hello answers the joiner's FE session hello with the FF hello: the host and the joiner as a two row player table, the host under the
// Wii U PID of the hosting player's own Wii U identity and the name the host's console wrote into the game (the joiner's row is its own).
func (s *Session) hello(m []byte) {
	if len(m) < 110 {
		s.Trace.Logf("WIIU", "short hello, %d bytes", len(m))
		return
	}
	name := cstr(m[85:109])
	ff := lce.Tmpl("hello_ff.bin")
	copy(ff[1:21], s.hostID[:])
	binary.BigEndian.PutUint32(ff[29:], s.HostPID)
	putStr(ff[33:57], s.HostName)
	copy(ff[81:101], m[5:25])
	binary.BigEndian.PutUint32(ff[109:], s.JoinerPID)
	putStr(ff[113:137], name)
	s.mu.Lock()
	copy(s.joinerUID[:], m[5:25])
	s.haveJoiner = true
	ready := s.ready
	s.mu.Unlock()
	if ready {
		s.pair()
	}
	s.SetState("wiiu-hello", fmt.Sprintf("Wii U player %q pid %d", name, s.JoinerPID))
	s.pmu.Lock()
	s.wiiuSend(ff)
	s.pmu.Unlock()
}

// pair makes the ids of the two networks one player: the Wii U joiner's hello id and the PS3 identity we joined with, the PS3 host's id and
// the id the Wii U was shown for it.
func (s *Session) pair() {
	s.mu.Lock()
	j, h, ju, hu := s.joinerUID, s.hostID, s.ps3UID, s.hostUID
	have := s.haveJoiner
	s.mu.Unlock()
	if have {
		s.uids.Pair(j[:], ju[:])
	}
	if hu != [28]byte{} {
		s.uids.Pair(h[:], hu[:])
	}
}

// Ready is called by the PS3 leg when its hello has left: the game layer takes the host's bytes from now on, and what the Wii U sent
// meanwhile goes out.
func (s *Session) Ready(env ps3join.GameEnv) {
	s.mu.Lock()
	s.ps3, s.ps3UID, s.hostUID, s.name, s.leave = env.Write, env.UID, env.Host, env.Name, env.Leave
	s.ready = true
	s.mu.Unlock()
	s.pair()
	s.SetState("ps3-hello", fmt.Sprintf("joined as %s (small id %d), host %q", env.Name, env.Small, trimNUL(env.Host[:16])))
	s.wmu.Lock()
	q := s.wq
	s.wq = nil
	for _, b := range q {
		s.forward(b)
	}
	s.wmu.Unlock()
}

func (s *Session) fromWiiU(b []byte) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		s.wq = append(s.wq, bytes.Clone(b))
		return
	}
	s.forward(b)
}

// forward walks the console's bytes and writes the translation to the PS3 host. Caller holds wmu.
func (s *Session) forward(b []byte) {
	s.mu.Lock()
	ps3, name, uid, ugc := s.ps3, s.name, s.ps3UID, s.ugc
	s.mu.Unlock()
	pkts, rest, err := walk.Walk(append(s.wbuf, b...), walk.WiiU)
	s.wbuf = bytes.Clone(rest)
	var out []byte
	left := false
	for _, p := range pkts {
		if p.ID == 0x01 { // the login's own ids are the joiner's on the Wii U side: one player with the PS3 identity
			for _, o := range p.UIDs {
				if u := p.Raw[o : o+20]; !uniformFill(u) {
					s.uids.Pair(u, uid[:])
				}
			}
		}
		t := bytes.Clone(walk.Translate(p, walk.WiiU, walk.PS3, &s.uids))
		switch p.ID {
		case 0xE8: // the joiner's name is its online id on the host's network
			t = rename(t, p.Version+2, name)
		case 0x01:
			t = ps3join.FixLogin(rename(t, 5, name), uid, ugc)
		}
		s.Trace.Logf("WALK", "wiiu->ps3 %#02x %s, %d -> %d bytes", p.ID, walk.Name(p.ID), len(p.Raw), len(t))
		switch p.ID {
		case 0xE8:
			s.SetState("prelogin", "the Wii U's E8 went to the host")
		case 0x01:
			s.SetState("login", "the Wii U's 01 Login went to the host")
		case 0xFF:
			left = true
		}
		out = append(out, t...)
	}
	if len(out) > 0 {
		ps3(out)
	}
	if err != nil {
		s.Trace.Logf("WALK", "wii u stream: %v; the session ends, the raw bytes are in .wiiu.rx.bin", err)
		defer s.End("the Wii U's stream could not be walked: " + err.Error())
	}
	if left {
		defer s.End("the Wii U disconnected")
	}
}

// OnPS3 takes game bytes of the PS3 host (kind 1 and 2 messages, pieces cut anywhere): packets are walked, translated to the Wii U dialect and
// sent on its channels (chunks 0x33 on channel 2, the rest on channel 1).
func (s *Session) OnPS3(b []byte) {
	s.pmu.Lock()
	pkts, rest, err := walk.Walk(append(s.pbuf, b...), walk.PS3)
	s.pbuf = bytes.Clone(rest)
	var ch1 []byte
	flush := func() {
		if len(ch1) > 0 {
			s.wiiuChannel(1, ch1)
			ch1 = nil
		}
	}
	gone := false
	for _, p := range pkts {
		switch p.ID {
		case 0x02: // the host's own id, and the friends-only/UGC pair our 01 echoes
			s.mu.Lock()
			s.ugc = ps3join.NoteUGC(p)
			s.mu.Unlock()
			for _, o := range p.UIDs {
				s.mu.Lock()
				s.hostUID = [28]byte(p.Raw[o : o+28])
				s.mu.Unlock()
				s.pair()
			}
		case 0xFF:
			gone = true
			s.mu.Lock()
			s.hostSaidFF = true
			s.mu.Unlock()
		case 0xD6:
			s.SetState("in-world", "the host placed the player")
		}
		t := walk.Translate(p, walk.PS3, walk.WiiU, &s.uids)
		s.Trace.Logf("WALK", "ps3->wiiu %#02x %s, %d -> %d bytes", p.ID, walk.Name(p.ID), len(p.Raw), len(t))
		if p.ID == 0x33 {
			flush()
			s.wiiuChannel(2, t)
			continue
		}
		ch1 = append(ch1, t...)
	}
	flush()
	s.pmu.Unlock()
	if err != nil {
		s.Trace.Logf("WALK", "ps3 stream: %v; the session ends, the raw bytes are in .rx.bin", err)
		s.End("the PS3 host's stream could not be walked: " + err.Error())
	}
	if gone {
		s.End("the PS3 host disconnected us")
	}
}

// End ends the session once: the Wii U gets a disconnect (the host's own, when it sent one, else reason 3) and its peer is closed, the PS3
// leg leaves the room.
func (s *Session) End(reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.state = "ended"
	cancel, leave, saidFF := s.cancel, s.leave, s.hostSaidFF
	s.mu.Unlock()
	s.Log.Info("bridge session", "state", "ended", "detail", reason)
	s.Trace.Logf("STATE", "ended %s", reason)
	if leave != nil {
		leave(reason)
	}
	if cancel != nil {
		cancel()
	}
	if !saidFF {
		s.pmu.Lock()
		s.wiiuChannel(1, binary.BigEndian.AppendUint32([]byte{0xFF}, disconnectReason))
		s.pmu.Unlock()
	}
	s.finishWiiU()
}

// finishWiiU closes the Wii U peer a moment after the last message left (a closed peer sends nothing more).
func (s *Session) finishWiiU() {
	s.mu.Lock()
	drop := s.drop
	s.drop = nil
	s.mu.Unlock()
	if drop != nil {
		time.AfterFunc(500*time.Millisecond, drop)
	}
}

// Idle is how long the Wii U has been silent (its clock tick comes every 50 ms while it is connected).
func (s *Session) Idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastRx)
}

// Layer is the session as the PS3 joiner's game layer. The joiner builds it while its hello is being sent, so nothing is written here; Start
// (right after the hello left) makes the session ready and lets what the Wii U already sent go out. The host's bytes go to OnPS3.
func (s *Session) Layer(env ps3join.GameEnv) ps3join.GameLayer { return layer{s, env} }

type layer struct {
	s   *Session
	env ps3join.GameEnv
}

func (l layer) Start()          { l.s.Ready(l.env) }
func (l layer) OnGame(b []byte) { l.s.OnPS3(b) }
func (l layer) Stop()           {}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func trimNUL(b []byte) string { return strings.TrimRight(cstr(b), "\x00") }

func putStr(dst []byte, s string) {
	for i := range dst {
		dst[i] = 0
	}
	copy(dst, s)
}

func uniformFill(b []byte) bool {
	for _, c := range b {
		if c != b[0] {
			return false
		}
	}
	return len(b) > 0 && (b[0] == 0 || b[0] == 0xff)
}

// rename replaces the UTF-16 string (short count, units) at off by name.
func rename(b []byte, off int, name string) []byte {
	if off+2 > len(b) {
		return b
	}
	n := int(int16(binary.BigEndian.Uint16(b[off:])))
	end := off + 2
	if n > 0 {
		end += 2 * n
	}
	if end > len(b) {
		return b
	}
	u := utf16.Encode([]rune(name))
	out := append(bytes.Clone(b[:off]), binary.BigEndian.AppendUint16(nil, uint16(len(u)))...)
	for _, c := range u {
		out = binary.BigEndian.AppendUint16(out, c)
	}
	return append(out, b[end:]...)
}
