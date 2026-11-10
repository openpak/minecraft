package lce

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/lce/walk"
)

// Client is the console joiner's game layer: what the Quay joiner sent the real Wii U host on 2026-11-07 (game_j2h_*, walked
// packet by packet with internal/lce/walk), driven by what the host sends. Order: FE hello -> (FF) -> E8 pre-login -> (02) ->
// 01 login -> (01 login response ... D6 position) -> 87 accept-teleport + 0D position, then a 0B position and an E2 keep-alive
// every second and an empty channel 3 tick every 50 ms, for as long as the session is held.
type Client struct {
	Send    func(msg []byte) // one reliable game message (pia5.Joiner.SendGame)
	Name    string
	PID     uint32
	HostPID uint32
	Log     *slog.Logger

	mu        sync.Mutex
	stats     ClientStats
	ch1       []byte
	walkErr   error
	x, y, z   float64
	keepalive uint32
	done      chan struct{}
	closeOnce sync.Once
}

// ClientStats is what the host has sent so far.
type ClientStats struct {
	HostHello, PreLogin, Login, InWorld bool
	HostName                            string
	EntityID                            int32
	Packets, Chunks                     int
	Disconnect                          string // the host's FF reason, if it sent one
	X, Y, Z                             float64
}

// uid is the 20-byte player id the joiner puts in its hello and login. The capture's three ids share 16 bytes and differ in
// the last 4 (FE ...da622534, login ...d05cfa58 and ...d1ca00ff); what those 4 are is not known, so the templates keep theirs
// and only the shared 16 are ours, derived from the PID so a player keeps the same id across sessions.
func (c *Client) uid16() []byte {
	h := sha1.Sum([]byte("openpak lce wiiu " + strconv.FormatUint(uint64(c.PID), 10)))
	return h[:16]
}

// Hello is the FE session hello (template hello_fe.bin: the capture's, its name cleared): 20-byte id at 5, the NUL-terminated
// ASCII name at 85 (the host reads up to 24 bytes; the capture has stack bytes after the NUL, kept), the host's PID at 237 and 253.
func (c *Client) Hello() []byte {
	fe := tmpl("hello_fe.bin")
	copy(fe[5:21], c.uid16())
	n := copy(fe[85:108], c.Name)
	fe[85+n] = 0
	binary.BigEndian.PutUint32(fe[237:], c.HostPID)
	binary.BigEndian.PutUint32(fe[253:], c.HostPID)
	return fe
}

// PreLogin is E8: the net version and the player name.
func (c *Client) PreLogin() []byte {
	return append([]byte{0xE8, NetVersion >> 8, NetVersion & 0xff}, utf16be(c.Name)...)
}

// LoginPacket is 01: client version 316, the name, then the capture's remaining fields with both PlayerUIDs ours.
func (c *Client) LoginPacket() []byte {
	b := append([]byte{0x01, 0, 0, 1, 0x3c}, utf16be(c.Name)...)
	b = append(b, tmpl("login_client.bin")...)
	p, err := walk.Next(b, walk.WiiU)
	if err != nil {
		panic(err) // the template walks; a failure is a programming error
	}
	for _, o := range p.UIDs {
		copy(b[o:o+16], c.uid16())
	}
	return b
}

func (c *Client) packets(b ...[]byte) {
	var all []byte
	for _, p := range b {
		all = append(all, p...)
	}
	c.Send(append([]byte{0, chPackets}, all...))
}

// Start sends the hello and runs the tick loop until Stop.
func (c *Client) Start() {
	c.done = make(chan struct{})
	c.Send(c.Hello())
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for n := 1; ; n++ {
			select {
			case <-c.done:
				return
			case <-tick.C:
			}
			c.Send([]byte{0, chTick})
			if n%20 != 0 {
				continue
			}
			c.mu.Lock()
			in, x, y, z, k := c.stats.InWorld, c.x, c.y, c.z, c.keepalive
			c.mu.Unlock()
			if in {
				c.packets(keepAlive(k), position(x, y, z, true))
			}
		}
	}()
}

func (c *Client) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// Stop ends the tick loop.
func (c *Client) Stop() { c.closeOnce.Do(func() { close(c.done) }) }

// Stats returns a copy of the counters.
func (c *Client) Stats() ClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.X, s.Y, s.Z = c.x, c.y, c.z
	return s
}

// E2 ServerboundKeepAlive: a VarInt. The captured joiner sent `e2 00` every second; the host's only 00 keep-alive carried
// token 0. ponytail: echoes the last token seen; check a longer capture if a host ever sends another value.
func keepAlive(token uint32) []byte {
	return binary.AppendUvarint([]byte{0xE2}, uint64(token))
}

// 0B: three doubles and the ground flag (26 bytes).
func position(x, y, z float64, ground bool) []byte {
	b := []byte{0x0B}
	for _, v := range []float64{x, y, z} {
		b = binary.BigEndian.AppendUint64(b, math.Float64bits(v))
	}
	if ground {
		return append(b, 1)
	}
	return append(b, 0)
}

// OnGame handles one message from the host.
func (c *Client) OnGame(msg []byte) {
	switch {
	case len(msg) > 0 && msg[0] == 0xFF:
		c.mu.Lock()
		c.stats.HostHello = true
		if len(msg) >= 57 {
			c.stats.HostName = ascii(msg[33:57])
		}
		c.mu.Unlock()
		c.log().Info("host hello", "host", c.Stats().HostName)
		c.packets(c.PreLogin())
	case len(msg) >= 2 && msg[0] == 0 && msg[1] == chPackets:
		c.channel1(msg[2:])
	case len(msg) >= 3 && msg[0] == 0 && msg[1] == chChunks:
		c.mu.Lock()
		if msg[2] == 0x33 {
			c.stats.Chunks++
		}
		c.mu.Unlock()
	}
}

func (c *Client) channel1(b []byte) {
	c.mu.Lock()
	if c.walkErr != nil {
		c.mu.Unlock()
		return
	}
	pkts, rest, err := walk.Walk(append(c.ch1, b...), walk.WiiU)
	c.ch1 = append([]byte(nil), rest...)
	if err != nil && !errors.Is(err, walk.ErrShort) {
		c.walkErr = err
		c.log().Warn("host stream: walk stopped", "err", err, "after_packets", c.stats.Packets)
	}
	var out [][]byte
	for _, p := range pkts {
		c.stats.Packets++
		r := p.Raw
		switch p.ID {
		case 0x00: // keep-alive, int token
			if len(r) >= 5 {
				c.keepalive = binary.BigEndian.Uint32(r[1:])
			}
		case 0x02: // pre-login reply: log in
			if !c.stats.PreLogin {
				c.stats.PreLogin = true
				out = append(out, c.LoginPacket())
			}
		case 0x01: // login response: int entity id first
			if !c.stats.Login && len(r) >= 5 {
				c.stats.Login = true
				c.stats.EntityID = int32(binary.BigEndian.Uint32(r[1:]))
			}
		case 0xD6: // the host places the player (PlayerPosition read 021ccd1c): x y z, u8 has-look, [yaw pitch], VarInt teleport
			// id; answered with 87 <id> + 0D (x y z, the host's yaw and pitch, ground 0)
			look := make([]byte, 8)
			o := 26
			if r[25] != 0 {
				copy(look, r[26:34])
				o = 34
			}
			c.x = math.Float64frombits(binary.BigEndian.Uint64(r[1:]))
			c.y = math.Float64frombits(binary.BigEndian.Uint64(r[9:]))
			c.z = math.Float64frombits(binary.BigEndian.Uint64(r[17:]))
			id, _ := binary.Uvarint(r[o:])
			pos := append(append([]byte{0x0D}, r[1:25]...), look...)
			out = append(out, append(binary.AppendUvarint([]byte{0x87}, id), append(pos, 0)...))
			c.stats.InWorld = true
		case 0xFF:
			if len(r) >= 5 {
				c.stats.Disconnect = fmt.Sprintf("%#x", binary.BigEndian.Uint32(r[1:]))
			}
		}
	}
	c.mu.Unlock()
	for _, o := range out {
		c.packets(o)
	}
}
