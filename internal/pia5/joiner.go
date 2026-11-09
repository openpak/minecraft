package pia5

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Joiner is the joiner side of the same handshake, enough to drive the host in tests and in `minecraft joinprobe`: it is built
// from the captured joiner's messages (docs/provenance.md P3-P6), not a general PIA client.
type Joiner struct {
	Conn    net.PacketConn
	Host    netip.AddrPort
	Cipher  *Cipher
	PID     uint32 // our PID
	HostPID uint32
	Self    netip.AddrPort // the address we claim in station locations
	// OnGame receives each complete game message; Joined closes once the mesh is up.
	OnGame func([]byte)
	Joined chan struct{}
	// Debug, when set, is told about every message received.
	Debug func(proto, port uint8, n int)

	mu        sync.Mutex
	cid       uint32
	nonce     [8]byte
	start     time.Time
	pktID     uint16
	connected bool
	ourAck    uint32
	reqSent   bool
	hostConn  uint8
	game      *stream
	mesh      *stream
	hostTimer uint16
	respSent  bool
}

func (j *Joiner) ms() uint64 { return uint64(time.Since(j.start).Milliseconds()) }

func (j *Joiner) emit(msgs ...Message) {
	hd := Header{Timer: uint16(j.ms()), Echo: j.hostTimer}
	if j.connected {
		j.pktID++
		hd.Conn, hd.ID = 0xc4, j.pktID
	}
	binary.BigEndian.PutUint32(j.nonce[4:], binary.BigEndian.Uint32(j.nonce[4:])+1)
	j.Conn.WriteTo(j.Cipher.Seal(hd, j.nonce, msgs), net.UDPAddrFromAddrPort(j.Host))
}

func (j *Joiner) loc(nat uint8) Loc {
	return Loc{Addr: j.Self, PID: j.PID, CID: j.cid, RVCID: 4, NATType: nat}
}

func (j *Joiner) station(p []byte) Message {
	return Message{Flags: 1, Proto: ProtoStation, Payload: p}
}

func (j *Joiner) connMsg(proto, port uint8, p []byte) Message {
	return Message{Flags: 1, Dest: 1, Src: uint64(j.PID), Proto: proto, Port: port, Payload: p}
}

// Run drives the handshake and reads until ctx ends.
func (j *Joiner) Run(ctx context.Context) error {
	j.start = time.Now()
	if j.Joined == nil {
		j.Joined = make(chan struct{})
	}
	j.game = &stream{proto: ProtoReliable, sendSeq: FirstSeq, recvNext: FirstSeq}
	j.mesh = &stream{proto: ProtoSession, port: 1, sendSeq: FirstSeq, recvNext: FirstSeq}
	var r [12]byte
	rand.Read(r[:])
	j.cid = binary.BigEndian.Uint32(r[:4])
	copy(j.nonce[:], r[4:])
	binary.BigEndian.PutUint16(j.nonce[:], 4)
	j.ourAck = binary.BigEndian.Uint32(r[8:])
	go func() {
		<-ctx.Done()
		j.Conn.Close()
	}()
	j.mu.Lock()
	j.sendRequest()
	j.mu.Unlock()
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for ctx.Err() == nil {
			<-t.C
			j.mu.Lock()
			if !j.connected {
				j.sendRequest() // UDP: repeat until the host has answered
			}
			for _, s := range []*stream{j.game, j.mesh} {
				for i := range s.flight {
					j.emit(j.connMsg(s.proto, s.port, Reliable{Flags: s.flight[i].flags, Seq: s.flight[i].seq, Ack: s.recvNext, Payload: s.flight[i].data}.Marshal()))
				}
			}
			j.mu.Unlock()
		}
	}()
	buf := make([]byte, 2048)
	for {
		n, _, err := j.Conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		hd, _, msgs, err := j.Cipher.Open(append([]byte(nil), buf[:n]...))
		if err != nil {
			continue
		}
		j.mu.Lock()
		j.hostTimer = hd.Timer
		for _, m := range msgs {
			j.message(m)
		}
		j.mu.Unlock()
	}
}

func (j *Joiner) sendRequest() {
	req := connRequest{ConnID: 0xc4, Version: 8, DestPID: j.HostPID, Public: j.loc(3), Private: j.loc(0), AckID: j.ourAck}
	j.emit(j.station(req.marshal()))
}

func (j *Joiner) message(m Message) {
	if j.Debug != nil {
		j.Debug(m.Proto, m.Port, len(m.Payload))
	}
	switch m.Proto {
	case ProtoStation:
		p := m.Payload
		switch {
		case len(p) >= 91 && p[0] == 1 && p[3] == 1: // the host's inverse request
			r, ok := parseConnRequest(p)
			if !ok {
				return
			}
			j.hostConn = r.ConnID
			j.emit(j.station(stationAck(r.AckID)))
			if !j.respSent {
				j.respSent = true
				resp := connResponse(8, r.Public.PID, r.Public.CID, j.ourAck+1)
				j.emit(j.station(resp))
			}
		case len(p) >= 255 && p[0] == 2: // the host's response
			j.emit(j.station(stationAck(binary.BigEndian.Uint32(p[251:]))))
			if !j.connected {
				join := append([]byte{1, 0xfd, 0, 0}, j.Self.Addr().AsSlice()...)
				join = binary.BigEndian.AppendUint16(join, j.Self.Port())
				join = binary.BigEndian.AppendUint32(join, j.ourAck+2)
				j.emit(Message{Flags: 1, Proto: ProtoSession, Payload: join})
			}
		}
	case ProtoSession:
		if m.Port == 0 && len(m.Payload) >= 164 && m.Payload[0] == 2 { // join response
			if !j.connected {
				j.connected = true
				j.emit(j.station(stationAck(binary.BigEndian.Uint32(m.Payload[len(m.Payload)-4:]))))
			}
			return
		}
		if m.Port == 1 {
			j.reliable(j.mesh, m)
		}
	case ProtoReliable:
		j.reliable(j.game, m)
	}
}

func (j *Joiner) reliable(s *stream, m Message) {
	r, ok := ParseReliable(m.Payload)
	if !ok {
		return
	}
	keep := s.flight[:0]
	for _, sg := range s.flight {
		if int32(sg.seq-r.Ack) >= 0 {
			keep = append(keep, sg)
		}
	}
	s.flight = keep
	if r.Flags&1 != 0 && r.Seq == s.recvNext {
		s.recvNext++
		s.asm = append(s.asm, r.Payload...)
		if r.Flags&2 != 0 {
			msg := s.asm
			s.asm = nil
			if s == j.mesh {
				select {
				case <-j.Joined:
				default:
					close(j.Joined)
				}
			} else if j.OnGame != nil {
				j.OnGame(msg)
			}
		}
	}
	if r.Flags&1 != 0 {
		j.emit(j.connMsg(m.Proto, m.Port, Reliable{Ack: s.recvNext}.Marshal()))
	}
}

// SendGame queues one game message (at most one segment).
func (j *Joiner) SendGame(msg []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.game
	sg := segment{seq: s.sendSeq, flags: 3, data: append([]byte(nil), msg...)}
	s.sendSeq++
	s.flight = append(s.flight, sg)
	j.emit(j.connMsg(ProtoReliable, 0, Reliable{Flags: 3, Seq: sg.seq, Ack: s.recvNext, Payload: sg.data}.Marshal()))
}
