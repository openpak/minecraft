// Package lce is the console (LCE) game layer of the Wii U adapter: it terminates one joiner's PIA reliable game stream, speaks
// the console's session handshake and packets, and drives a Java connection that holds the player in the shared world. Every
// wire fact here is one the 2026-11-07 capture confirms (docs/provenance.md); bytes the capture shows but this package does not
// understand are replayed from the embedded templates (internal/lce/tmpl) with only the fields named in session.go patched.
package lce

import (
	"embed"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/openpak/minecraft/internal/javamc"
	"github.com/openpak/minecraft/internal/lcechunk"
	"github.com/openpak/minecraft/internal/pia5"
)

//go:embed tmpl/*.bin
var tmplFS embed.FS

func tmpl(name string) []byte {
	b, err := tmplFS.ReadFile("tmpl/" + name)
	if err != nil {
		panic(err)
	}
	return append([]byte(nil), b...)
}

// Tmpl returns a copy of an embedded template.
func Tmpl(name string) []byte { return tmpl(name) }

// Config is what a session needs from its service.
type Config struct {
	JavaAddr     string // the Java server, host:port
	HostName     string // the host name the console shows
	HostID       [20]byte
	ViewDistance int
	Log          *slog.Logger
	// Joined and Left record the session in the database.
	Joined func(name string)
	Left   func(reason string, chunks int64)
}

// NetVersion is the net version both sides of the capture announce (0x0881).
const NetVersion = 0x0881

const (
	chPackets = 1
	chChunks  = 2
	chTick    = 3
	pieceMax  = 5120 // game bytes per reliable message
)

// worldSync template: where the joiner's spawn position (three big-endian doubles x, y, z) sits.
const spawnOffset = 18352

// Session is one console player.
type Session struct {
	cfg     Config
	peer    *pia5.Peer
	hostID  [20]byte
	hostPID uint32
	log     *slog.Logger

	mu         sync.Mutex
	name       string
	javaName   string
	java       *javamc.Client
	ready      bool
	closed     bool
	lastRx     time.Time
	chunks     int64
	sent       map[[2]int]time.Time
	lastPos    time.Time
	cols       map[[2]int]*javamc.ChunkColumn
	dirty      map[[2]int]bool
	px, py, pz float64
	ground     bool
	havePos    bool
	done       chan struct{}
}

// NewSession attaches to a PIA peer.
func NewSession(cfg Config, peer *pia5.Peer, hostPID uint32) *Session {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Session{cfg: cfg, peer: peer, hostID: cfg.HostID, hostPID: hostPID, log: cfg.Log.With("peer", peer.Addr.String()),
		lastRx: time.Now(), cols: map[[2]int]*javamc.ChunkColumn{}, dirty: map[[2]int]bool{}, done: make(chan struct{})}
	peer.OnGame = s.onGame
	go s.watch()
	go s.sender()
	return s
}

func (s *Session) watch() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.mu.Lock()
			idle := time.Since(s.lastRx)
			s.mu.Unlock()
			if idle > 30*time.Second {
				s.Close("timeout")
				return
			}
		}
	}
}

// Close ends the session.
func (s *Session) Close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	j := s.java
	n := s.chunks
	close(s.done)
	s.mu.Unlock()
	if j != nil {
		j.Close()
	}
	s.log.Info("session ended", "reason", reason, "name", s.name, "chunks", n)
	if s.cfg.Left != nil {
		s.cfg.Left(reason, n)
	}
}

func (s *Session) onGame(msg []byte) {
	s.mu.Lock()
	s.lastRx = time.Now()
	s.mu.Unlock()
	switch {
	case len(msg) > 0 && msg[0] == 0xFE:
		s.hello(msg)
	case len(msg) >= 2 && msg[0] == 0:
		switch msg[1] {
		case chPackets:
			s.packets(msg[2:])
		case chTick:
		default:
			s.log.Info("unhandled channel", "ch", msg[1], "len", len(msg))
		}
	default:
		s.log.Info("unhandled game message", "len", len(msg))
	}
}

// ascii reads a zero padded field.
func ascii(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func put(dst []byte, s string) {
	for i := range dst {
		dst[i] = 0
	}
	copy(dst, s)
}

// hello answers the joiner's FE session hello with the FF hello: the host and the joiner as a two row player table.
func (s *Session) hello(m []byte) {
	if len(m) < 110 {
		s.log.Warn("short hello", "len", len(m))
		return
	}
	s.name = ascii(m[85:109])
	s.javaName = javaName(s.name)
	ff := tmpl("hello_ff.bin")
	copy(ff[1:21], s.hostID[:])
	binary.BigEndian.PutUint32(ff[29:], s.hostPID)
	put(ff[33:57], s.cfg.HostName)
	copy(ff[81:101], m[5:25])
	binary.BigEndian.PutUint32(ff[109:], s.peer.PID())
	put(ff[113:137], s.name)
	s.log.Info("hello", "name", s.name, "pid", s.peer.PID())
	s.peer.Send(ff)
	if s.cfg.Joined != nil {
		s.cfg.Joined(s.name)
	}
}

func javaName(n string) string {
	var b strings.Builder
	for _, r := range n {
		if r < 128 && (r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "Console"
	}
	return b.String()[:min(b.Len(), 16)]
}

func utf16be(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := binary.BigEndian.AppendUint16(nil, uint16(len(u)))
	for _, c := range u {
		b = binary.BigEndian.AppendUint16(b, c)
	}
	return b
}

// send puts game bytes on a channel, cut into pieces of pieceMax.
func (s *Session) send(ch byte, data []byte) {
	for first := true; len(data) > 0 || first; first = false {
		n := min(len(data), pieceMax)
		s.peer.Send(append([]byte{0, ch}, data[:n]...))
		data = data[n:]
	}
}

// packets walks the joiner's channel 1 message. The capture shows exactly one packet per client message.
func (s *Session) packets(b []byte) {
	if len(b) == 0 {
		return
	}
	switch b[0] {
	case 0xE8: // pre-login: short net version, string player name
		s.log.Info("pre-login", "version", fmt.Sprintf("%#x", binary.BigEndian.Uint16(b[1:])))
		s.send(chPackets, []byte{0, 0, 0, 0, 0}) // the host sends a keep alive first
		tail := tmpl("prelogin.bin")[21:]
		copy(tail[6:22], s.hostID[:16])
		out := append([]byte{2, NetVersion >> 8, NetVersion & 0xff}, utf16be(s.cfg.HostName)...)
		s.send(chPackets, append(out, tail...))
	case 0x01: // login: start the Java side, answer once it has a position
		go s.start()
	case 0x0B: // position: three doubles and the ground flag
		if len(b) >= 26 {
			s.position(b[1:])
		}
	case 0x87: // wrapper: 87 01 then a packet; 0D = position and look
		if len(b) >= 36 && b[1] == 1 && b[2] == 0x0D {
			s.position(b[3:])
		}
	case 0xE2, 0xCD: // single byte payload / client command: not mapped
	default:
		s.log.Info("unhandled client packet", "id", fmt.Sprintf("%#x", b[0]), "len", len(b))
	}
}

func (s *Session) position(b []byte) {
	x := math.Float64frombits(binary.BigEndian.Uint64(b))
	y := math.Float64frombits(binary.BigEndian.Uint64(b[8:]))
	z := math.Float64frombits(binary.BigEndian.Uint64(b[16:]))
	s.mu.Lock()
	s.px, s.py, s.pz, s.havePos = x, y, z, true
	s.ground = b[len(b)-1] != 0
	j := s.java
	ok := s.ready && time.Since(s.lastPos) > 45*time.Millisecond
	if ok {
		s.lastPos = time.Now()
	}
	g := s.ground
	s.mu.Unlock()
	if j != nil && ok {
		j.Position(x, y, z, 0, 0, g)
	}
}

// start connects to the Java server and, with its spawn position, sends the world sync.
func (s *Session) start() {
	s.mu.Lock()
	if s.java != nil || s.closed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	j, err := javamc.Dial(s.cfg.JavaAddr, s.javaName)
	if err != nil {
		s.log.Error("java connect", "err", err)
		s.Close("java connect failed")
		return
	}
	s.mu.Lock()
	s.java = j
	s.mu.Unlock()
	s.log.Info("java connected", "as", s.javaName, "uuid", j.UUID)
	s.javaLoop(j)
}

func (s *Session) javaLoop(j *javamc.Client) {
	sentSync := false
	for {
		id, data, err := j.Read()
		if err != nil {
			s.Close("java closed: " + err.Error())
			return
		}
		switch id {
		case javamc.KeepAlive:
			t, _, _ := javamc.ReadVarInt(data)
			j.KeepAlive(t)
		case javamc.JoinGame:
			j.Settings(s.cfg.ViewDistance) // not before: the server switches state just after the login success
		case javamc.PosAndLook:
			r := javamc.NewReader(data)
			x, y, z := r.Double(), r.Double(), r.Double()
			if !sentSync && r.Err() == nil {
				sentSync = true
				s.worldSync(x, y, z)
				s.mu.Lock()
				s.ready = true
				s.px, s.py, s.pz = x, y, z
				s.mu.Unlock()
			}
		case javamc.TimeUpdate:
			r := javamc.NewReader(data)
			age, day := r.Long(), r.Long()
			s.timeUpdate(age, day)
		case javamc.ChunkData, javamc.ChunkBulk:
			cols, err := javamc.Columns(id, data, true)
			if err != nil {
				s.log.Warn("chunk packet", "err", err)
				continue
			}
			for _, c := range cols {
				s.column(c)
			}
		case javamc.MultiBlock, javamc.BlockChange:
			s.blockChange(id, data)
		case javamc.Disconnect:
			s.Close("java disconnect")
			return
		}
	}
}

// worldSync is the login answer and the sync packet the capture's host sent after it, with the spawn position set.
func (s *Session) worldSync(x, y, z float64) {
	s.log.Info("spawn", "x", x, "y", y, "z", z)
	s.send(chPackets, tmpl("login.bin"))
	w := tmpl("world_sync.bin")
	binary.BigEndian.PutUint64(w[spawnOffset:], math.Float64bits(x))
	binary.BigEndian.PutUint64(w[spawnOffset+8:], math.Float64bits(y))
	binary.BigEndian.PutUint64(w[spawnOffset+16:], math.Float64bits(z))
	s.send(chPackets, w)
}

func (s *Session) timeUpdate(age, day int64) {
	b := []byte{0x04}
	b = binary.BigEndian.AppendUint64(b, uint64(age))
	b = binary.BigEndian.AppendUint64(b, uint64(day))
	s.mu.Lock()
	ok := s.ready
	s.mu.Unlock()
	if ok {
		s.send(chPackets, b)
	}
}

// chunkPacket builds a channel 2 message: id 0x33, the full column header, the compressed column and two zero bytes.
func chunkPacket(cx, cz int, c *lcechunk.Column) []byte {
	z := lcechunk.Pack(c)
	b := []byte{0x33, 1}
	b = binary.BigEndian.AppendUint32(b, uint32(int32(cx*16)))
	b = append(b, 0, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(int32(cz*16)))
	b = append(b, 15, 255, 15)
	b = binary.BigEndian.AppendUint32(b, uint32(len(z))) // dimension 0 in the top two bits
	b = append(b, z...)
	return append(b, 0, 0)
}

func (s *Session) column(c *javamc.ChunkColumn) {
	k := [2]int{c.X, c.Z}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(c.Sections) == 0 && c.GroundUp && len(c.Biomes) == 0 { // unload
		delete(s.cols, k)
		delete(s.dirty, k)
		return
	}
	s.cols[k] = c
	s.dirty[k] = true
}

// sender streams dirty columns to the console, nearest to the player first, paced like the console host (about 25 a second).
func (s *Session) sender() {
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		s.mu.Lock()
		if !s.ready || len(s.dirty) == 0 {
			s.mu.Unlock()
			continue
		}
		cx, cz := int(math.Floor(s.px))>>4, int(math.Floor(s.pz))>>4
		var best [2]int
		bd := math.MaxInt
		for k := range s.dirty {
			if d := (k[0]-cx)*(k[0]-cx) + (k[1]-cz)*(k[1]-cz); d < bd {
				best, bd = k, d
			}
		}
		delete(s.dirty, best)
		c := s.cols[best]
		s.mu.Unlock()
		if c == nil {
			continue
		}
		s.send(chChunks, chunkPacket(best[0], best[1], Convert(c)))
		s.mu.Lock()
		s.chunks++
		s.mu.Unlock()
	}
}

// blockChange applies Java block changes to the held column and marks it for a resend: the console's own block change packet
// layout is not in the capture (unconfirmed U4), a full column resend is.
func (s *Session) blockChange(id int, data []byte) {
	r := javamc.NewReader(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == javamc.BlockChange {
		x, y, z := r.Position()
		v := r.VarInt()
		s.set(x, y, z, uint16(v))
		return
	}
	cx, cz := int(r.Int()), int(r.Int())
	n := r.VarInt()
	for i := 0; i < n && r.Err() == nil; i++ {
		h := r.Byte()
		y := int(r.Byte())
		v := r.VarInt()
		s.set(cx*16+int(h>>4), y, cz*16+int(h&15), uint16(v))
	}
}

func (s *Session) set(x, y, z int, v uint16) {
	k := [2]int{x >> 4, z >> 4}
	c := s.cols[k]
	if c == nil || y < 0 || y > 255 {
		return
	}
	var sec *javamc.Section
	for i := range c.Sections {
		if c.Sections[i].Y == y>>4 {
			sec = &c.Sections[i]
		}
	}
	if sec == nil {
		c.Sections = append(c.Sections, javamc.Section{Y: y >> 4})
		sec = &c.Sections[len(c.Sections)-1]
		for i := range sec.SkyLight {
			sec.SkyLight[i] = 0xff
		}
	}
	sec.Blocks[((y&15)*16+(z&15))*16+(x&15)] = v
	s.dirty[k] = true
}
