package ps3p2p

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// The game's message layer on RUDP (spec §2.3): one RUDP message is `u16 len, u16 kind, payload[len]`, big-endian. Kind 1 is game bytes
// queued with the SendData flag clear and the 32-byte hello, kind 2 game bytes with the flag set (the receiver answers each with an empty
// kind-3 message `00 00 00 03`), the send queue cuts the game stream into pieces of at most MaxPiece bytes. A readable message of exactly
// 4 bytes is an ack whatever its kind. A message of 0x542 bytes plus its 4-byte header is longer than one RUDP segment (1,346), so RUDP
// delivers it in two or more segments (the second a bare 4-byte tail, seen on hardware): the receive side parses messages from the
// reassembled byte stream, never one message per segment.
const (
	KindPlain = 1
	KindAcked = 2
	KindAck   = 3
	MaxPiece  = 0x542 // 1,346 (EB 0xc8a0d4)
)

// Msgr is the message layer over one established Conn.
type Msgr struct {
	C *Conn
	// OnGame gets the payload of every kind 1 and kind 2 message (kind says which); OnAck every ack. Both may be nil.
	OnGame func(kind uint16, payload []byte)
	OnAck  func()
	// Trace sees every message in both directions ("rx"/"tx") with its kind and payload.
	Trace func(dir string, kind uint16, payload []byte, note string)

	mu       sync.Mutex
	inflight int    // kind-2 messages sent and not yet acked (conn+0xb8 in the game)
	pend     []byte // received stream bytes not yet a whole message
}

// Attach wires the Conn's payload callback to this layer.
func (m *Msgr) Attach() { m.C.OnPayload = m.recv }

// Inflight is the number of kind-2 messages sent and not yet acknowledged.
func (m *Msgr) Inflight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight
}

// Write sends one message exactly as given (no splitting); the hello and tests use it.
func (m *Msgr) Write(kind uint16, payload []byte, now time.Time) error {
	if len(payload) > 0xffff {
		return fmt.Errorf("ps3p2p: message of %d bytes", len(payload))
	}
	b := binary.BigEndian.AppendUint16(nil, uint16(len(payload)))
	b = binary.BigEndian.AppendUint16(b, kind)
	b = append(b, payload...)
	if kind == KindAcked {
		m.mu.Lock()
		m.inflight++
		m.mu.Unlock()
	}
	if m.Trace != nil {
		m.Trace("tx", kind, payload, "")
	}
	return m.C.Send(b, now)
}

// WriteGame cuts game bytes into MaxPiece pieces, one message each, kind 2 when counted.
func (m *Msgr) WriteGame(data []byte, counted bool, now time.Time) error {
	kind := uint16(KindPlain)
	if counted {
		kind = KindAcked
	}
	for len(data) > 0 {
		n := min(len(data), MaxPiece)
		if err := m.Write(kind, data[:n], now); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// Rx is one received message.
type Rx struct {
	Kind uint16
	P    []byte
}

// Split cuts a received byte stream into whole messages; rest is the unfinished tail (more segments to come). A message that claims more
// than MaxPiece bytes cannot be a boundary, and err says so (no sender cuts longer).
func Split(stream []byte) (msgs []Rx, rest []byte, err error) {
	for len(stream) >= 4 {
		n, kind := int(binary.BigEndian.Uint16(stream)), binary.BigEndian.Uint16(stream[2:])
		if n > MaxPiece {
			return msgs, stream, fmt.Errorf("ps3p2p: message of kind %d says %d bytes (max %d)", kind, n, MaxPiece)
		}
		if 4+n > len(stream) {
			break
		}
		msgs = append(msgs, Rx{kind, stream[4 : 4+n]})
		stream = stream[4+n:]
	}
	return msgs, stream, nil
}

func (m *Msgr) recv(b []byte) {
	m.mu.Lock()
	msgs, rest, err := Split(append(m.pend, b...))
	for i := range msgs {
		msgs[i].P = append([]byte(nil), msgs[i].P...)
	}
	m.pend = append([]byte(nil), rest...)
	if err != nil { // the stream is lost: start over at the next segment
		m.pend = nil
	}
	m.mu.Unlock()
	if err != nil {
		m.note("rx", 0, b, err.Error()+": stream dropped")
	}
	for _, g := range msgs {
		m.note("rx", g.Kind, g.P, "")
		if len(g.P) == 0 {
			m.mu.Lock()
			if m.inflight > 0 {
				m.inflight--
			}
			m.mu.Unlock()
			if m.OnAck != nil {
				m.OnAck()
			}
			continue
		}
		if g.Kind == KindAcked {
			m.Write(KindAck, nil, time.Now()) // not counted: a kind-3 write is an ack
		}
		if m.OnGame != nil {
			m.OnGame(g.Kind, g.P)
		}
	}
}

func (m *Msgr) note(dir string, kind uint16, p []byte, note string) {
	if m.Trace != nil && dir == "rx" {
		m.Trace(dir, kind, p, note)
	}
}
