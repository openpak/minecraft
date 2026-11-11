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
// 4 bytes is an ack whatever its kind.
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
	inflight int // kind-2 messages sent and not yet acked (conn+0xb8 in the game)
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

func (m *Msgr) recv(b []byte) {
	for len(b) > 0 {
		if len(b) < 4 {
			m.note("rx", 0, b, fmt.Sprintf("%d stray bytes after the last message", len(b)))
			return
		}
		n, kind := int(binary.BigEndian.Uint16(b)), binary.BigEndian.Uint16(b[2:])
		if 4+n > len(b) {
			m.note("rx", kind, b[4:], fmt.Sprintf("message says %d bytes, %d present: kept as received", n, len(b)-4))
			n = len(b) - 4
		}
		p := b[4 : 4+n]
		b = b[4+n:]
		m.note("rx", kind, p, "")
		if n == 0 {
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
		if kind == KindAcked {
			m.Write(KindAck, nil, time.Now()) // not counted: a kind-3 write is an ack
		}
		if m.OnGame != nil {
			m.OnGame(kind, append([]byte(nil), p...))
		}
	}
}

func (m *Msgr) note(dir string, kind uint16, p []byte, note string) {
	if m.Trace != nil && dir == "rx" {
		m.Trace(dir, kind, p, note)
	}
}
