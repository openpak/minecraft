package ps3p2p

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// RUDP is the librudp layer as far as the spec defines it (§2.1-2.2, matching2-protocol.md §26): a datagram is a sequence of aggregator
// frames, each `u16 (class<<14 | length incl. this header)`; muxmode 1 means class 2, which carries a u16 RUDP port (the joining console's pad
// index + 1, the same on both ends) before the RUDP packet. A packet is `type<<6|flags, window, seq, [ack], [ts], block, payload`:
// type 0 data, 1 ack/keepalive, 2 SYN or SYN+ACK, 3 reset. Both sides Initiate (simultaneous open), which Conn handles as in §26.
//
// Delivery here is reliable and in order (go-back-N), message oriented: one RUDP data payload is one game message. How exactly librudp
// retransmits is not decoded; the timings are guesses (guess.go).
const (
	typeData  = 0
	typeAck   = 1
	typeSyn   = 2
	typeReset = 3

	flagAck  = 0x20
	flagOpts = 0x10
	flagTS   = 0x04

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
	OnPayload     func([]byte) // one in-order data payload (one message)
	OnEstablished func()
	OnReset       func()

	mu       sync.Mutex
	state    int
	isn      uint16
	connID   uint32
	nextSeq  uint16 // next data seq to assign
	expect   uint16 // next data seq expected from the peer
	peerISN  uint16
	peerMSS  uint16
	peerConn uint32
	havePeer bool
	unacked  []seg
	queue    [][]byte
	lastSyn  time.Time
	lastTx   time.Time
	started  bool
}

type seg struct {
	seq     uint16
	payload []byte
	sent    time.Time
}

// NewConn prepares a context; nothing is sent until Start.
func NewConn(port uint16, isn uint16, connID uint32, out func([]byte)) *Conn {
	return &Conn{Port: port, Out: out, isn: isn, connID: connID, state: stSynSent, nextSeq: isn + 1}
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
	b := binary.BigEndian.AppendUint16(nil, GuessCapWord)
	b = binary.BigEndian.AppendUint16(b, GuessVerA)
	b = binary.BigEndian.AppendUint16(b, GuessVerB)
	b = binary.BigEndian.AppendUint32(b, c.connID)
	return binary.BigEndian.AppendUint16(b, GuessMSS)
}

func (c *Conn) synPkt() []byte {
	p := []byte{typeSyn << 6, GuessWindowByte, byte(c.isn >> 8), byte(c.isn)}
	return append(p, c.synBlock()...)
}

func (c *Conn) synAckPkt() []byte {
	p := []byte{typeSyn<<6 | flagAck, GuessWindowByte, byte(c.isn >> 8), byte(c.isn), byte(c.expect >> 8), byte(c.expect)}
	return append(p, c.synBlock()...)
}

// emptyAck is the pure acknowledgement and keepalive: a data packet with the ack flag and no payload, seq = the next one to send
// (it consumes none), ack = the next one expected (guess.go: GuessEmptyAckIsType0).
func (c *Conn) emptyAck() []byte {
	t := byte(typeData)
	if !GuessEmptyAckIsType0 {
		t = typeAck
	}
	return []byte{t<<6 | flagAck, GuessWindowByte, byte(c.nextSeq >> 8), byte(c.nextSeq), byte(c.expect >> 8), byte(c.expect)}
}

func (c *Conn) dataPkt(s seg) []byte {
	p := []byte{flagAck, GuessWindowByte, byte(s.seq >> 8), byte(s.seq), byte(c.expect >> 8), byte(c.expect)}
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

// Send queues one payload (one message); it goes out as the window allows. It never blocks.
func (c *Conn) Send(payload []byte, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != stEstab {
		return fmt.Errorf("ps3p2p: rudp not established")
	}
	c.queue = append(c.queue, append([]byte(nil), payload...))
	c.pump(now)
	return nil
}

func (c *Conn) pump(now time.Time) {
	for len(c.queue) > 0 && len(c.unacked) < GuessSendWindow {
		s := seg{seq: c.nextSeq, payload: c.queue[0], sent: now}
		c.queue = c.queue[1:]
		c.nextSeq++
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
	case stSynSent:
		if now.Sub(c.lastSyn) >= GuessSynRetry {
			c.lastSyn = now
			c.tx(c.synPkt(), now)
		}
	case stSynRcvd:
		if now.Sub(c.lastSyn) >= GuessSynRetry {
			c.lastSyn = now
			c.tx(c.synAckPkt(), now)
		}
	case stEstab:
		if len(c.unacked) > 0 && now.Sub(c.unacked[0].sent) >= GuessRTO {
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

// parse returns type, flags, seq, ack and the offset of the type block / payload (options skipped as the §26 decode reads them).
func parse(p []byte) (typ, flags byte, seq, ack uint16, off int, err error) {
	if len(p) < 4 {
		return 0, 0, 0, 0, 0, fmt.Errorf("short packet")
	}
	typ, flags = p[0]>>6, p[0]&0x3f
	seq = binary.BigEndian.Uint16(p[2:])
	off = 4
	if flags&flagAck != 0 {
		if len(p) < off+2 {
			return 0, 0, 0, 0, 0, fmt.Errorf("short ack")
		}
		ack = binary.BigEndian.Uint16(p[off:])
		off += 2
	}
	if flags&flagTS != 0 {
		off += 2
	}
	if flags&flagOpts != 0 {
		for off < len(p) {
			t := p[off]
			off++
			if t == 0 {
				break
			}
			if off >= len(p) {
				return 0, 0, 0, 0, 0, fmt.Errorf("short option")
			}
			off += 1 + int(p[off])
			if t&0x80 == 0 {
				break
			}
		}
	}
	if off > len(p) {
		return 0, 0, 0, 0, 0, fmt.Errorf("options overrun")
	}
	return typ, flags, seq, ack, off, nil
}

// Describe is the one-line form of an RUDP packet for the recorder.
func Describe(p []byte) string {
	typ, flags, seq, ack, off, err := parse(p)
	if err != nil {
		return "unparsable: " + err.Error()
	}
	s := fmt.Sprintf("type=%d flags=%#02x win=%#02x seq=%d", typ, flags, p[1], seq)
	if flags&flagAck != 0 {
		s += fmt.Sprintf(" ack=%d", ack)
	}
	switch typ {
	case typeSyn:
		if len(p) >= off+12 {
			s += fmt.Sprintf(" SYN cap=%#04x verA=%#04x verB=%#04x connid=%#08x mss=%d", binary.BigEndian.Uint16(p[off:]), binary.BigEndian.Uint16(p[off+2:]),
				binary.BigEndian.Uint16(p[off+4:]), binary.BigEndian.Uint32(p[off+6:]), binary.BigEndian.Uint16(p[off+10:]))
		}
	default:
		s += fmt.Sprintf(" payload=%d", len(p)-off)
	}
	return s
}

func after(a, b uint16) bool { return int16(a-b) > 0 } // a is later than b

func (c *Conn) packet(p []byte, now time.Time) (deliver [][]byte, estab, reset bool) {
	typ, flags, seq, ack, off, err := parse(p)
	if err != nil {
		c.note("rx", "unparsable packet: "+err.Error(), p)
		return
	}
	if c.Trace != nil {
		c.Trace("rx", Describe(p), p)
	}
	switch typ {
	case typeSyn:
		if len(p) < off+12 {
			c.note("rx", "short SYN block", p)
			return
		}
		c.peerConn = binary.BigEndian.Uint32(p[off+6:])
		c.peerMSS = binary.BigEndian.Uint16(p[off+10:])
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
