package ps3p2p

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// RUDP is the librudp layer (§2.1-2.2, matching2-protocol.md §26 and §43): a datagram is a sequence of aggregator frames, each
// `u16 (class<<14 | length incl. this header)`; muxmode 1 means class 2, which carries a u16 RUDP port (the joining console's pad
// index + 1, the same on both ends) before the RUDP packet. A packet is `type<<6|flags, window, seq, [ack], [ts], block, [options],
// payload`: type 0 data, 1 ack/keepalive, 2 SYN or SYN+ACK (block: cap word, verA, verB, connId, MSS), 3 reset (block: reason u16, own
// connId). Both sides Initiate (simultaneous open), which Conn handles as librudp's state machine does (§43). SEEN ON HARDWARE: the
// SYN layout, class 2 + port 1, window byte, MSS, cap word, version words, the 1/2/4/8 s retransmit timer, the reset (guess.go).
//
// Delivery here is reliable and in order (go-back-N), message oriented: one RUDP data payload is one game message. The data-phase
// timings are still guesses (guess.go).
const (
	typeData  = 0
	typeAck   = 1
	typeSyn   = 2
	typeReset = 3

	flagAck  = 0x20
	flagOpts = 0x10
	flagTS   = 0x04
	flagMore = 0x02 // more segments of this message follow

	stSynSent = 2 // as the §26 decode numbers them
	stSynRcvd = 3
	stEstab   = 4
)

// Conn is one RUDP context (one peer, one port). Out receives a complete plaintext datagram (one aggregator frame) to be sealed and
// sent; Handle takes a decrypted datagram. Tick must be called regularly (Run does).
type Conn struct {
	Port uint16 // RUDP port = pad index + 1
	Out  func(datagram []byte)

	// Trace, if set, sees every packet in both directions with a one-line description (dir is "rx" or "tx").
	Trace         func(dir, desc string, pkt []byte)
	OnPayload     func([]byte) // one in-order data segment (a piece of the byte stream, see above)
	OnEstablished func()
	OnReset       func()
	OnDead        func() // data went unacknowledged for GuessDeadAfter: the peer is gone (called once, no more data is sent)

	mu       sync.Mutex
	state    int
	isn      uint16
	connID   uint32
	nextSeq  uint16 // next data seq to assign
	expect   uint16 // next data seq expected from the peer
	peerISN  uint16
	peerMSS  uint16
	peerConn uint32
	cap      uint16        // capability word we send: CapWord until the console's own is seen, then mirrored
	rto      time.Duration // SYN / SYN+ACK retransmit timer, doubling
	havePeer bool
	unacked  []seg
	queue    []seg
	dataRTO  time.Duration // current data retransmission timeout: GuessRTO, doubling per unanswered round, back to GuessRTO on progress
	progress time.Time     // last time the peer acked new data (or the oldest unacked segment was sent)
	dead     bool
	lastSyn  time.Time
	lastTx   time.Time
	started  bool
}

type seg struct {
	seq     uint16
	payload []byte
	more    bool
	sent    time.Time
}

// NewConn prepares a context; nothing is sent until Start.
func NewConn(port uint16, isn uint16, connID uint32, out func([]byte)) *Conn {
	return &Conn{Port: port, Out: out, isn: isn, connID: connID, state: stSynSent, nextSeq: isn + 1, cap: CapWord, rto: SynRetryFirst, dataRTO: GuessRTO}
}

func (c *Conn) frame(pkt []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, 0x8000|uint16(4+len(pkt)))
	b = binary.BigEndian.AppendUint16(b, c.Port)
	return append(b, pkt...)
}

func (c *Conn) tx(pkt []byte, now time.Time) {
	c.lastTx = now
	if c.Trace != nil {
		c.Trace("tx", Describe(pkt), pkt)
	}
	c.Out(c.frame(pkt))
}

func (c *Conn) synBlock() []byte {
	b := binary.BigEndian.AppendUint16(nil, c.cap)
	b = binary.BigEndian.AppendUint16(b, VerA)
	b = binary.BigEndian.AppendUint16(b, VerB)
	b = binary.BigEndian.AppendUint32(b, c.connID)
	return binary.BigEndian.AppendUint16(b, MSS)
}

func (c *Conn) synPkt() []byte {
	p := []byte{typeSyn << 6, WindowByte, byte(c.isn >> 8), byte(c.isn)}
	return append(p, c.synBlock()...)
}

func (c *Conn) synAckPkt() []byte {
	p := []byte{typeSyn<<6 | flagAck, WindowByte, byte(c.isn >> 8), byte(c.isn), byte(c.expect >> 8), byte(c.expect)}
	return append(p, c.synBlock()...)
}

// emptyAck is the pure acknowledgement and keepalive: a data packet with the ack flag and no payload, seq = the next one to send
// (it consumes none), ack = the next one expected (guess.go: GuessEmptyAckIsType0).
func (c *Conn) emptyAck() []byte {
	t := byte(typeData)
	if !GuessEmptyAckIsType0 {
		t = typeAck
	}
	return []byte{t<<6 | flagAck, WindowByte, byte(c.nextSeq >> 8), byte(c.nextSeq), byte(c.expect >> 8), byte(c.expect)}
}

func (c *Conn) dataPkt(s seg) []byte {
	f := byte(flagAck)
	if s.more {
		f |= flagMore
	}
	p := []byte{f, WindowByte, byte(s.seq >> 8), byte(s.seq), byte(c.expect >> 8), byte(c.expect)}
	return append(p, s.payload...)
}

// Start sends the first SYN (the Initiate of the simultaneous open).
func (c *Conn) Start(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	c.lastSyn = now
	c.tx(c.synPkt(), now)
}

// Established reports whether the handshake is done.
func (c *Conn) Established() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == stEstab
}

// PeerMSS is the MSS in the peer's SYN block (0 before it).
func (c *Conn) PeerMSS() uint16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peerMSS
}

// Send queues one message; it is cut into segments of at most MSS-0x40 bytes (all but the last flagged "more") and goes out as the window
// allows. It never blocks.
func (c *Conn) Send(payload []byte, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != stEstab || c.dead {
		return fmt.Errorf("ps3p2p: rudp not established or dead")
	}
	max := int(MSS) - 0x40
	if m := int(c.peerMSS); m >= 0x80 && m < int(MSS) { // the peer's MSS, clamped as librudp does
		max = m - 0x40
	}
	for first := true; len(payload) > 0 || first; first = false {
		n := min(len(payload), max)
		c.queue = append(c.queue, seg{payload: append([]byte(nil), payload[:n]...), more: n < len(payload)})
		payload = payload[n:]
	}
	c.pump(now)
	return nil
}

func (c *Conn) pump(now time.Time) {
	for len(c.queue) > 0 && len(c.unacked) < GuessSendWindow {
		s := c.queue[0]
		c.queue = c.queue[1:]
		s.seq, s.sent = c.nextSeq, now
		c.nextSeq++
		if len(c.unacked) == 0 {
			c.progress = now
		}
		c.unacked = append(c.unacked, s)
		c.tx(c.dataPkt(s), now)
	}
}

// Tick drives SYN repetition, retransmission and keepalives.
func (c *Conn) Tick(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return
	}
	switch c.state {
	case stSynSent, stSynRcvd:
		// librudp's timer: 1 s, doubling, capped at 16 s (seen: 1, 2, 4, 8 s)
		if now.Sub(c.lastSyn) >= c.rto {
			c.lastSyn = now
			if c.rto *= 2; c.rto > SynRetryMax {
				c.rto = SynRetryMax
			}
			if c.state == stSynSent {
				c.tx(c.synPkt(), now)
			} else {
				c.tx(c.synAckPkt(), now)
			}
		}
	case stEstab:
		if c.dead {
			return
		}
		if len(c.unacked) > 0 && now.Sub(c.progress) >= GuessDeadAfter {
			c.dead, c.queue, c.unacked = true, nil, nil
			if c.OnDead != nil {
				go c.OnDead()
			}
			return
		}
		if len(c.unacked) > 0 && now.Sub(c.unacked[0].sent) >= c.dataRTO {
			// go-back-N resend of what is in flight, then wait twice as long (librudp-style backoff, capped): an unanswered peer is
			// not flooded, and a stalled one is declared dead after GuessDeadAfter
			c.dataRTO = min(2*c.dataRTO, GuessRTOMax)
			for i := range c.unacked {
				c.unacked[i].sent = now
				c.tx(c.dataPkt(c.unacked[i]), now)
			}
		} else if now.Sub(c.lastTx) >= GuessKeepalive {
			c.tx(c.emptyAck(), now)
		}
	}
}

// Handle feeds one decrypted datagram. Problems are traced and end that datagram only; nothing here returns an error to abort on.
func (c *Conn) Handle(d []byte, now time.Time) {
	var deliver [][]byte
	var estab, reset bool
	c.mu.Lock()
	for len(d) >= 2 {
		h := binary.BigEndian.Uint16(d)
		class, l := int(h>>14), int(h&0x7ff)
		if l < 2 || l > len(d) {
			c.note("rx", fmt.Sprintf("bad frame length %d of %d", l, len(d)), d)
			break
		}
		p := d[2:l]
		d = d[l:]
		switch class {
		case 2:
			if len(p) < 2 {
				c.note("rx", "short class-2 frame", p)
				continue
			}
			if port := binary.BigEndian.Uint16(p); port != c.Port {
				c.note("rx", fmt.Sprintf("frame for rudp port %d (ours %d), ignored", port, c.Port), p)
				continue
			}
			p = p[2:]
		case 1:
			c.note("rx", "class-1 frame (muxmode 0?), treated as ours", p)
		default:
			c.note("rx", fmt.Sprintf("class %d frame, ignored", class), p)
			continue
		}
		dl, e, r := c.packet(p, now)
		deliver = append(deliver, dl...)
		estab = estab || e
		reset = reset || r
	}
	c.mu.Unlock()
	if estab && c.OnEstablished != nil {
		c.OnEstablished()
	}
	for _, p := range deliver {
		if c.OnPayload != nil {
			c.OnPayload(p)
		}
	}
	if reset && c.OnReset != nil {
		c.OnReset()
	}
}

func (c *Conn) note(dir, s string, pkt []byte) {
	if c.Trace != nil {
		c.Trace(dir, s, pkt)
	}
}

// parse returns type, flags, seq, ack, the offset of the type block and the offset of the payload (librudp's order, R 0xddb8: header,
// window, seq, ack, ts, type block, options, payload).
func parse(p []byte) (typ, flags byte, seq, ack uint16, blk, off int, err error) {
	fail := func(s string) (byte, byte, uint16, uint16, int, int, error) {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("%s", s)
	}
	if len(p) < 4 {
		return fail("short packet")
	}
	typ, flags = p[0]>>6, p[0]&0x3f
	seq = binary.BigEndian.Uint16(p[2:])
	off = 4
	if flags&flagAck != 0 {
		if len(p) < off+2 {
			return fail("short ack")
		}
		ack = binary.BigEndian.Uint16(p[off:])
		off += 2
	}
	if flags&flagTS != 0 {
		off += 2
	}
	blk = off
	switch typ {
	case typeSyn:
		off += 12
	case typeReset:
		off += 6
	}
	if off > len(p) {
		return fail("short type block")
	}
	if flags&flagOpts != 0 {
		for off < len(p) {
			t := p[off]
			off++
			if t&0x7f != 0 {
				if off >= len(p) {
					return fail("short option")
				}
				off += 1 + int(p[off])
			}
			if t&0x80 != 0 {
				break
			}
		}
	}
	if off > len(p) {
		return fail("options overrun")
	}
	return typ, flags, seq, ack, blk, off, nil
}

// Describe is the one-line form of an RUDP packet for the recorder.
func Describe(p []byte) string {
	typ, flags, seq, ack, blk, off, err := parse(p)
	if err != nil {
		return fmt.Sprintf("unparsable (%v) %x", err, p)
	}
	name := [...]string{"DATA", "ACK", "SYN", "RESET"}[typ]
	if typ == typeSyn && flags&flagAck != 0 {
		name = "SYN+ACK"
	}
	s := fmt.Sprintf("%s flags=%#02x win=%#02x seq=%#04x", name, flags, p[1], seq)
	if flags&flagAck != 0 {
		s += fmt.Sprintf(" ack=%#04x", ack)
	}
	switch typ {
	case typeSyn:
		s += fmt.Sprintf(" cap=%#04x verA=%#04x verB=%#04x connid=%#08x mss=%d", binary.BigEndian.Uint16(p[blk:]), binary.BigEndian.Uint16(p[blk+2:]),
			binary.BigEndian.Uint16(p[blk+4:]), binary.BigEndian.Uint32(p[blk+6:]), binary.BigEndian.Uint16(p[blk+10:]))
	case typeReset:
		s += fmt.Sprintf(" reason=%d connid=%#08x", p[blk+1], binary.BigEndian.Uint32(p[blk+2:]))
	}
	if off < len(p) {
		s += fmt.Sprintf(" payload(%d)=%x", len(p)-off, p[off:])
	}
	return s
}

func after(a, b uint16) bool { return int16(a-b) > 0 } // a is later than b

func (c *Conn) packet(p []byte, now time.Time) (deliver [][]byte, estab, reset bool) {
	typ, flags, seq, ack, blk, off, err := parse(p)
	if err != nil {
		c.note("rx", "unparsable packet: "+err.Error(), p)
		return
	}
	if c.Trace != nil {
		c.Trace("rx", Describe(p), p)
	}
	switch typ {
	case typeSyn:
		if verA, verB := binary.BigEndian.Uint16(p[blk+2:]), binary.BigEndian.Uint16(p[blk+4:]); verA < VerA || verB > VerB {
			c.note("rx", "SYN version words refused (librudp aborts locally, reason 4): ignored", p)
			return
		}
		c.cap = binary.BigEndian.Uint16(p[blk:]) // the console's flags: its check wants ours equal
		c.peerConn = binary.BigEndian.Uint32(p[blk+6:])
		c.peerMSS = binary.BigEndian.Uint16(p[blk+10:])
		if flags&flagAck != 0 {
			if ack != c.isn+1 {
				c.note("rx", fmt.Sprintf("SYN+ACK acks %d, want %d: ignored", ack, c.isn+1), p)
				return
			}
			if !c.havePeer {
				c.peerISN, c.expect, c.havePeer = seq, seq+1, true
			}
			c.tx(c.emptyAck(), now) // completes the peer's SYN-received state
			if c.state != stEstab {
				c.state = stEstab
				estab = true
				c.pump(now)
			}
			return
		}
		c.peerISN, c.expect, c.havePeer = seq, seq+1, true
		if c.state != stEstab {
			c.state = stSynRcvd
		}
		c.lastSyn = now
		c.tx(c.synAckPkt(), now)
	case typeData, typeAck:
		if flags&flagAck != 0 {
			if c.state == stSynRcvd && ack == c.isn+1 {
				c.state = stEstab
				estab = true
			}
			for len(c.unacked) > 0 && after(ack, c.unacked[0].seq) {
				c.unacked = c.unacked[1:]
				c.dataRTO, c.progress = GuessRTO, now
			}
			c.pump(now)
		}
		if typ == typeData && off < len(p) {
			switch {
			case seq == c.expect:
				c.expect++
				deliver = append(deliver, append([]byte(nil), p[off:]...))
				c.tx(c.emptyAck(), now)
			default:
				// a repeat or a gap: ask for what we expect (go-back-N)
				c.tx(c.emptyAck(), now)
			}
		}
	case typeReset:
		reset = true
	}
	return
}
