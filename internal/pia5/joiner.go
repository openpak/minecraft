package pia5

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// Joiner is the joiner side of the station handshake, built from the captured console joiner's messages (docs/provenance.md
// P3-P6, J1-J4), not a general PIA client. It drives the service host in tests and `minecraft joinprobe`, and joins a real console
// host in `minecraft joinconsole` (Targets set): there it first does the NAT probe exchange a console joiner does, keeps the
// station alive with RTT and clock requests, and answers the host's RTT and probe requests.
type Joiner struct {
	Conn    net.PacketConn
	Host    netip.AddrPort // the host's address; with Targets set, the target whose probe reply came first
	Cipher  *Cipher
	PID     uint32 // our PID
	HostPID uint32
	Self    netip.AddrPort // the address we claim in station locations (the public one)
	Private netip.AddrPort // the private location's address; zero: Self
	// CID and RVCID are our station's ids (the station URL nex gave the host carries the same ones). Zero: CID random, RVCID 4.
	CID, RVCID uint32
	// Targets, when set, are the host station's addresses (public and private): the joiner probes them and sends its connection
	// request only after a probe reply, as a console joiner does (capture 12:39:52.268 to .303).
	Targets []netip.AddrPort
	// OnGame receives each complete game message (called without the joiner's lock); Joined closes once the mesh is up.
	OnGame func([]byte)
	Joined chan struct{}
	// Debug, when set, is told about every message received; OnTraversed about the probe reply that picked Host.
	Debug       func(proto, port uint8, n int)
	OnTraversed func(from netip.AddrPort)

	mu        sync.Mutex
	nonce     [8]byte
	start     time.Time
	pktID     uint16
	connected bool
	traversed bool
	ourAck    uint32
	hostConn  uint8
	game      *stream
	mesh      *stream
	hostTimer uint16
	respSent  bool
	pending   [][]byte
	rtts      int
	lastRTT   time.Time
	lastClock time.Time
}

func (j *Joiner) ms() uint64 { return uint64(time.Since(j.start).Milliseconds()) }

func (j *Joiner) emitTo(to netip.AddrPort, msgs ...Message) {
	hd := Header{Timer: uint16(j.ms()), Echo: j.hostTimer}
	if j.connected {
		j.pktID++
		hd.Conn, hd.ID = 0xc4, j.pktID
	}
	binary.BigEndian.PutUint32(j.nonce[4:], binary.BigEndian.Uint32(j.nonce[4:])+1)
	j.Conn.WriteTo(j.Cipher.Seal(hd, j.nonce, msgs), net.UDPAddrFromAddrPort(to))
}

func (j *Joiner) emit(msgs ...Message) { j.emitTo(j.Host, msgs...) }

func (j *Joiner) loc(nat uint8) Loc {
	a := j.Self
	if nat == 0 && j.Private.IsValid() {
		a = j.Private
	}
	return Loc{Addr: a, PID: j.PID, CID: j.CID, RVCID: j.RVCID, NATType: nat}
}

func (j *Joiner) station(p []byte) Message {
	return Message{Flags: 1, Proto: ProtoStation, Payload: p}
}

func (j *Joiner) connMsg(proto, port uint8, p []byte) Message {
	return Message{Flags: 1, Dest: 1, Src: uint64(j.PID), Proto: proto, Port: port, Payload: p}
}

// probe sends the two type 0 probes the captured joiner sent each target (flags 9, no source id, port 1).
func (j *Joiner) probe(to netip.AddrPort) {
	for i := uint64(0); i < 2; i++ {
		j.emitTo(to, Message{Flags: 9, Proto: ProtoProbe, Port: 1, Payload: probePayload(j.RVCID, probeRequest, uint64(j.CID)<<8+i)})
	}
}

func (j *Joiner) init() {
	j.start = time.Now()
	if j.Joined == nil {
		j.Joined = make(chan struct{})
	}
	if j.RVCID == 0 {
		j.RVCID = 4
	}
	j.game = &stream{proto: ProtoReliable, sendSeq: FirstSeq, recvNext: FirstSeq}
	j.mesh = &stream{proto: ProtoSession, port: 1, sendSeq: FirstSeq, recvNext: FirstSeq}
	var r [12]byte
	rand.Read(r[:])
	if j.CID == 0 {
		j.CID = binary.BigEndian.Uint32(r[:4])
	}
	copy(j.nonce[:], r[4:])
	binary.BigEndian.PutUint16(j.nonce[:], uint16(j.RVCID)) // a station's nonce starts with its RVCID (capture: host 0003, joiner 0004)
	j.ourAck = binary.BigEndian.Uint32(r[8:])
	j.traversed = len(j.Targets) == 0
}

// Run drives the handshake and reads until ctx ends.
func (j *Joiner) Run(ctx context.Context) error {
	j.init()
	go func() {
		<-ctx.Done()
		j.Conn.Close()
	}()
	j.mu.Lock()
	j.kick()
	j.mu.Unlock()
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for n := 1; ctx.Err() == nil; n++ {
			<-t.C
			j.mu.Lock()
			if n%3 == 0 { // UDP: repeat until answered, every 300 ms
				j.kick()
				for _, s := range []*stream{j.game, j.mesh} {
					for i := range s.flight {
						j.emit(j.connMsg(s.proto, s.port, Reliable{Flags: s.flight[i].flags, Seq: s.flight[i].seq, Ack: s.recvNext, Payload: s.flight[i].data}.Marshal()))
					}
				}
			}
			if j.connected {
				j.keepalives()
			}
			j.mu.Unlock()
		}
	}()
	buf := make([]byte, 2048)
	for {
		n, from, err := j.Conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		hd, _, msgs, err := j.Cipher.Open(append([]byte(nil), buf[:n]...))
		if err != nil {
			continue
		}
		ap := from.(*net.UDPAddr).AddrPort()
		j.handle(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), hd, msgs)
	}
}

func (j *Joiner) handle(from netip.AddrPort, hd Header, msgs []Message) {
	j.mu.Lock()
	j.hostTimer = hd.Timer
	for _, m := range msgs {
		j.message(from, m)
	}
	deliver := j.pending
	j.pending = nil
	j.mu.Unlock()
	for _, msg := range deliver {
		if j.OnGame != nil {
			j.OnGame(msg)
		}
	}
}

// kick sends what the current phase repeats: probes until a target answered, then the connection request until the host has.
func (j *Joiner) kick() {
	switch {
	case !j.traversed:
		for _, t := range j.Targets {
			j.probe(t)
		}
	case !j.connected:
		j.sendRequest()
	}
}

// keepalives is the captured joiner's station traffic: an RTT request every 0.2 s for the first ten, then every 2 s, and a
// clock request every second (pia_plain_joiner_to_host: 58/0 byte 3 = 0 at 0.45, 0.66 ... 2.36, 4.38; 1c/0 at 0.45, 1.46 ...).
func (j *Joiner) keepalives() {
	gap := 2 * time.Second
	if j.rtts < 10 {
		gap = 200 * time.Millisecond
	}
	if time.Since(j.lastRTT) >= gap {
		j.lastRTT = time.Now()
		j.rtts++
		ping := make([]byte, 16)
		binary.BigEndian.PutUint64(ping[8:], j.ms())
		j.emit(j.connMsg(ProtoRTT, 0, ping))
	}
	if time.Since(j.lastClock) >= time.Second {
		j.lastClock = time.Now()
		j.emit(j.connMsg(ProtoClock, 0, binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, j.ms()), 0)))
	}
}

func (j *Joiner) sendRequest() {
	req := connRequest{ConnID: 0xc4, Version: 8, DestPID: j.HostPID, Public: j.loc(3), Private: j.loc(0), AckID: j.ourAck}
	j.emit(j.station(req.marshal()))
}

func (j *Joiner) message(from netip.AddrPort, m Message) {
	if j.Debug != nil {
		j.Debug(m.Proto, m.Port, len(m.Payload))
	}
	switch m.Proto {
	case ProtoProbe:
		if len(m.Payload) < 16 {
			return
		}
		switch {
		case m.Port == 1 && m.Payload[4] == probeRequest: // the host's probe: a type 1 reply echoing its token (capture 12:39:52.018)
			j.emitTo(from, Message{Flags: 9, Proto: ProtoProbe, Port: 2, Payload: probePayload(j.RVCID, probeReply, binary.BigEndian.Uint64(m.Payload[8:16]))})
			// The host's probe shows where its PIA socket really is. The public station URL NEX holds can carry the port of the
			// console's NEX socket instead (real Wii U 2026-11-11: URL port 5224, PIA probes from 5254), so probe the sender too.
			if !j.traversed && len(j.Targets) > 0 && !slices.Contains(j.Targets, from) {
				j.Targets = append(j.Targets, from)
				j.probe(from)
			}
		case m.Port == 2 && m.Payload[4] == probeReply && !j.traversed: // traversal done: connect, 6 ms later in the capture
			j.traversed = true
			j.Host = from
			if j.OnTraversed != nil {
				j.OnTraversed(from)
			}
			j.sendRequest()
		}
	case ProtoRTT:
		if len(m.Payload) >= 16 && m.Payload[3] == 0 {
			out := append([]byte(nil), m.Payload[:16]...)
			out[3] = 1
			j.emit(j.connMsg(ProtoRTT, m.Port, out))
		}
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
			} else {
				j.pending = append(j.pending, msg)
			}
		}
	}
	if r.Flags&1 != 0 {
		j.emit(j.connMsg(m.Proto, m.Port, Reliable{Ack: s.recvNext}.Marshal()))
	}
}

// SendGame queues one game message, cut into segments of segMax.
func (j *Joiner) SendGame(msg []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := j.game
	for len(msg) > 0 {
		n := min(len(msg), segMax)
		fl := uint16(1)
		if n == len(msg) {
			fl |= 2
		}
		sg := segment{seq: s.sendSeq, flags: fl, data: append([]byte(nil), msg[:n]...)}
		s.sendSeq++
		s.flight = append(s.flight, sg)
		j.emit(j.connMsg(ProtoReliable, 0, Reliable{Flags: fl, Seq: sg.seq, Ack: s.recvNext, Payload: sg.data}.Marshal()))
		msg = msg[n:]
	}
}
