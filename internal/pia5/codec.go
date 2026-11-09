// Package pia5 is the PIA packet layer Minecraft: Wii U Edition speaks (PIA 5.7 to 5.10 format, AES-128-GCM), as read from the
// 2026-11-07 capture (docs/provenance.md rows P1-P9). Common/terraria's internal/pia is the ECB+HMAC (up to 5.6) cousin.
package pia5

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

// Magic starts every packet.
const Magic = 0x32AB9864

// Protocol type bytes (message header offset 19) seen in the capture.
const (
	ProtoStation  = 0x14 // connection request / response / ack
	ProtoSession  = 0x18 // mesh: port 0 join, port 1 reliable mesh updates
	ProtoClock    = 0x1c // session clock
	ProtoProbe    = 0x34 // NAT probe, port 1 request, port 2 answer
	ProtoRTT      = 0x58 // round trip, byte 3 0 = request, 1 = answer
	ProtoReliable = 0x7c // the game channel, port 0
)

// Header is the 12 bytes ahead of the nonce.
type Header struct {
	Conn  uint8  // 0 during the handshake, then the id from the connection request
	ID    uint16 // packet id: 0 during the handshake, then 1, 2, 3 ... per direction
	Timer uint16 // sender timer, ms
	Echo  uint16 // the peer's timer, echoed
}

// Message is one PIA message: 24 byte header and payload, padded to 4.
type Message struct {
	Flags   uint8
	Dest    uint64 // destination station bitmap (connected) or 0
	Src     uint64 // source constant id (the sender's PID) or 0
	Proto   uint8
	Port    uint8
	Payload []byte
}

// Cipher holds the key for one gathering.
type Cipher struct {
	aead cipher.AEAD
	gid  [3]byte
}

// NewCipher derives the packet key: the first 16 bytes of the NEX matchmake session key; gathering is the NEX gathering id (its
// low 24 bits go into the nonce).
func NewCipher(sessionKey []byte, gathering uint32) (*Cipher, error) {
	if len(sessionKey) < 16 {
		return nil, errors.New("pia5: session key shorter than 16 bytes")
	}
	b, err := aes.NewCipher(sessionKey[:16])
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: a, gid: [3]byte{byte(gathering >> 16), byte(gathering >> 8), byte(gathering)}}, nil
}

func (c *Cipher) nonce(conn uint8, n8 []byte) []byte {
	return append([]byte{conn, c.gid[0], c.gid[1], c.gid[2]}, n8...)
}

func marshalMessages(msgs []Message) []byte {
	var pl []byte
	for _, m := range msgs {
		pl = append(pl, m.Flags)
		pl = binary.BigEndian.AppendUint16(pl, uint16(len(m.Payload)))
		pl = binary.BigEndian.AppendUint64(pl, m.Dest)
		pl = binary.BigEndian.AppendUint64(pl, m.Src)
		pl = append(pl, m.Proto, m.Port, 0, 0, 0)
		pl = append(pl, m.Payload...)
		for len(pl)%4 != 0 {
			pl = append(pl, 0)
		}
	}
	for len(pl)%16 != 0 {
		pl = append(pl, 0xFF)
	}
	return pl
}

// Seal builds an encrypted packet. nonce is the 8 byte header nonce (the station counts it up per packet).
func (c *Cipher) Seal(h Header, nonce [8]byte, msgs []Message) []byte {
	out := make([]byte, 0, 36+len(msgs)*64)
	out = binary.BigEndian.AppendUint32(out, Magic)
	out = append(out, 2, h.Conn)
	out = binary.BigEndian.AppendUint16(out, h.ID)
	out = binary.BigEndian.AppendUint16(out, h.Timer)
	out = binary.BigEndian.AppendUint16(out, h.Echo)
	out = append(out, nonce[:]...)
	sealed := c.aead.Seal(nil, c.nonce(h.Conn, nonce[:]), marshalMessages(msgs), nil) // ciphertext || tag
	n := len(sealed) - 16
	out = append(out, sealed[n:]...)
	return append(out, sealed[:n]...)
}

var ErrPacket = errors.New("pia5: bad packet")

// Open decrypts a packet and splits its messages.
func (c *Cipher) Open(b []byte) (Header, [8]byte, []Message, error) {
	var h Header
	var n8 [8]byte
	if len(b) < 36+16 || binary.BigEndian.Uint32(b) != Magic || b[4] != 2 || (len(b)-36)%16 != 0 {
		return h, n8, nil, ErrPacket
	}
	h = Header{Conn: b[5], ID: binary.BigEndian.Uint16(b[6:]), Timer: binary.BigEndian.Uint16(b[8:]), Echo: binary.BigEndian.Uint16(b[10:])}
	copy(n8[:], b[12:20])
	ct := append(append([]byte(nil), b[36:]...), b[20:36]...)
	pl, err := c.aead.Open(nil, c.nonce(h.Conn, n8[:]), ct, nil)
	if err != nil {
		return h, n8, nil, err
	}
	var msgs []Message
	for o := 0; o+24 <= len(pl) && pl[o] != 0xFF; {
		size := int(binary.BigEndian.Uint16(pl[o+1:]))
		if o+24+size > len(pl) {
			return h, n8, msgs, ErrPacket
		}
		msgs = append(msgs, Message{Flags: pl[o], Dest: binary.BigEndian.Uint64(pl[o+3:]), Src: binary.BigEndian.Uint64(pl[o+11:]),
			Proto: pl[o+19], Port: pl[o+20], Payload: append([]byte(nil), pl[o+24:o+24+size]...)})
		o += (24 + size + 3) &^ 3
	}
	return h, n8, msgs, nil
}
