package pia5

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// Loc is a station location (35 bytes): the public or private address plus the identities NEX knows the station by.
type Loc struct {
	Addr            netip.AddrPort
	PID, CID, RVCID uint32
	NATType         uint8 // 3 on a public location, 0 on a private one
}

const locLen = 35

func (l Loc) marshal() []byte {
	b := make([]byte, locLen)
	a := l.Addr.Addr().As4()
	copy(b, a[:])
	binary.BigEndian.PutUint16(b[4:], l.Addr.Port())
	binary.BigEndian.PutUint32(b[10:], l.PID)
	binary.BigEndian.PutUint32(b[14:], l.CID)
	binary.BigEndian.PutUint32(b[18:], l.RVCID)
	copy(b[22:], []byte{1, 0x0f, 0, 1, 2, l.NATType, 0}) // url type, sid, stream, NAT mapping, NAT filter, NAT type, probe init
	return b
}

func parseLoc(b []byte) Loc {
	return Loc{Addr: netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[0:4])), binary.BigEndian.Uint16(b[4:])),
		PID: binary.BigEndian.Uint32(b[10:]), CID: binary.BigEndian.Uint32(b[14:]), RVCID: binary.BigEndian.Uint32(b[18:]), NATType: b[27]}
}

// connRequest is a station connection request (type 1): 91 bytes, `inverse` set on the answer to the peer's own request.
type connRequest struct {
	ConnID, Version uint8
	Inverse         bool
	DestPID         uint32 // the PID of the station the request is addressed to, as the sender knows it
	DestCID         uint32
	DestConn        uint8
	Public, Private Loc
	AckID           uint32
}

func (r connRequest) marshal() []byte {
	b := []byte{1, r.ConnID, r.Version, 0}
	if r.Inverse {
		b[3] = 1
	}
	b = binary.BigEndian.AppendUint64(b, uint64(r.DestPID))
	b = binary.BigEndian.AppendUint32(b, r.DestCID)
	b = append(b, r.DestConn)
	b = append(b, r.Public.marshal()...)
	b = append(b, r.Private.marshal()...)
	return binary.BigEndian.AppendUint32(b, r.AckID)
}

func parseConnRequest(p []byte) (connRequest, bool) {
	if len(p) < 4+13+2*locLen+4 {
		return connRequest{}, false
	}
	return connRequest{ConnID: p[1], Version: p[2], Inverse: p[3] != 0, DestPID: binary.BigEndian.Uint32(p[8:]), DestCID: binary.BigEndian.Uint32(p[12:]),
		DestConn: p[16], Public: parseLoc(p[17:]), Private: parseLoc(p[17+locLen:]), AckID: binary.BigEndian.Uint32(p[17+2*locLen:])}, true
}

// connResponse is our station connection response (type 2): 255 bytes, the layout is only partly mapped; unmapped bytes are the
// captured console's (docs/provenance.md P5).
func connResponse(version uint8, peerPID, peerCID uint32, ack uint32) []byte {
	b := make([]byte, 255)
	copy(b, []byte{2, 0, version, 3}) // type, result ok, station version, platform Wii U
	binary.BigEndian.PutUint32(b[9:], peerPID)
	binary.BigEndian.PutUint32(b[13:], peerCID)
	copy(b[17:], "Friend")
	copy(b[52:], []byte{2, 1, 1, 1})
	b[178] = 0xff
	binary.BigEndian.PutUint32(b[251:], ack)
	return b
}

func stationAck(id uint32) []byte { return binary.BigEndian.AppendUint32([]byte{5, 0, 0, 0}, id) }

// Reliable is the 24 byte header of a reliable stream segment.
type Reliable struct {
	Flags   uint16 // 1 data, 2 last fragment
	Seq     uint32
	Ack     uint32 // next sequence expected
	Payload []byte
}

// FirstSeq is the first sequence of every reliable stream seen in the capture.
const FirstSeq = 0xFFFFF82F

func (r Reliable) Marshal() []byte {
	b := binary.BigEndian.AppendUint16(nil, r.Flags)
	b = binary.BigEndian.AppendUint16(b, uint16(len(r.Payload)))
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint32(b, r.Seq)
	b = binary.BigEndian.AppendUint32(b, r.Ack)
	b = binary.BigEndian.AppendUint64(b, 0)
	return append(b, r.Payload...)
}

func ParseReliable(p []byte) (Reliable, bool) {
	if len(p) < 24 {
		return Reliable{}, false
	}
	n := int(binary.BigEndian.Uint16(p[2:]))
	if 24+n > len(p) {
		return Reliable{}, false
	}
	return Reliable{Flags: binary.BigEndian.Uint16(p), Seq: binary.BigEndian.Uint32(p[8:]), Ack: binary.BigEndian.Uint32(p[12:]), Payload: p[24 : 24+n]}, true
}

const (
	segMax    = 1000 // game bytes per reliable segment (the UDP payload cap is 1204)
	window    = 48   // unacked segments in flight
	resendAge = 300 * time.Millisecond
)

type segment struct {
	seq   uint32
	flags uint16
	data  []byte
	sent  time.Time
}

// stream is one reliable stream (a protocol type and port) with one peer.
type stream struct {
	proto, port uint8
	sendSeq     uint32
	recvNext    uint32
	queue       []segment // not yet sent
	flight      []segment // sent, not acked
	asm         []byte    // fragments of the message being received
}

// Peer is one joiner.
type Peer struct {
	h                  *Host
	Addr               netip.AddrPort
	req                connRequest // the joiner's connection request
	haveReq            bool
	ourConn            uint8
	ourAck             uint32
	timer              uint16 // last timer seen from the peer
	pktID              uint16
	connected          bool     // join response sent: packets carry our connection id and rising ids
	pending            [][]byte // complete game messages waiting for their callback
	streams            map[[2]uint8]*stream
	respSent, invSent  bool
	lastRTT, lastClock time.Time
	rtts               int
	// OnGame receives each complete game message (one reliable message, the `00 channel` framing still on it).
	OnGame func(msg []byte)
}

// PID is the joiner's PID from its connection request.
func (p *Peer) PID() uint32 { return p.req.Public.PID }

// Out sends a packet's worth of messages to a peer. The default seals and writes to the host's socket; tests replace it.
type Out func(to netip.AddrPort, h Header, nonce [8]byte, msgs []Message)

// Host answers joiners as the station they believe hosts a gathering.
type Host struct {
	mu      sync.Mutex
	cipher  *Cipher
	Self    netip.AddrPort // public address
	PID     uint32         // the PID the gathering's station URL names
	CID     uint32
	RVCID   uint32
	start   time.Time
	nonce   [8]byte
	out     Out
	peers   map[netip.AddrPort]*Peer
	Log     *slog.Logger
	NewPeer func(p *Peer) // called for the first packet from a new address
}

func NewHost(c *Cipher, self netip.AddrPort, pid uint32, out Out) *Host {
	h := &Host{cipher: c, Self: self, PID: pid, RVCID: HostRVCID, start: time.Now(), out: out, peers: map[netip.AddrPort]*Peer{}, Log: slog.Default()}
	var r [8]byte
	rand.Read(r[:])
	h.CID = binary.BigEndian.Uint32(r[:4])
	copy(h.nonce[:], r[:])
	binary.BigEndian.PutUint16(h.nonce[:], uint16(h.RVCID))
	return h
}

func (h *Host) ms() uint64 { return uint64(time.Since(h.start).Milliseconds()) }

func (h *Host) pub() Loc {
	return Loc{Addr: h.Self, PID: h.PID, CID: h.CID, RVCID: h.RVCID, NATType: 3}
}
func (h *Host) priv() Loc { l := h.pub(); l.NATType = 0; return l }

// emit sends one packet; connected peers get their connection id and a packet id.
func (p *Peer) emit(msgs ...Message) {
	h := p.h
	hd := Header{Timer: uint16(h.ms()), Echo: p.timer}
	if p.connected {
		p.pktID++
		hd.Conn, hd.ID = p.ourConn, p.pktID
	}
	binary.BigEndian.PutUint32(h.nonce[4:], binary.BigEndian.Uint32(h.nonce[4:])+1)
	h.out(p.Addr, hd, h.nonce, msgs)
}

func (p *Peer) station(payload []byte) Message {
	return Message{Flags: 1, Src: uint64(p.h.PID), Proto: ProtoStation, Payload: payload}
}

func (p *Peer) conn(proto, port uint8, payload []byte) Message {
	return Message{Flags: 1, Dest: 2, Src: uint64(p.h.PID), Proto: proto, Port: port, Payload: payload}
}

func (h *Host) peer(a netip.AddrPort) *Peer {
	p := h.peers[a]
	if p == nil {
		p = &Peer{h: h, Addr: a, streams: map[[2]uint8]*stream{}, ourConn: 0xbb}
		h.peers[a] = p
		p.probe()
		if h.NewPeer != nil {
			h.NewPeer(p)
		}
	}
	return p
}

// probe sends the two NAT probe requests (type 0x34, port 1) a console host sends a joiner before anything else (capture, host
// side, first two packets: its RVCID, its timer, an 8 byte token). A joiner answers them and waits for them before it sends its
// connection request, so a host that only answers the joiner's own probes leaves the joiner stuck.
func (p *Peer) probe() {
	h := p.h
	for i := uint64(0); i < 2; i++ {
		out := make([]byte, 16)
		binary.BigEndian.PutUint32(out, h.RVCID)
		binary.BigEndian.PutUint32(out[4:], uint32(h.ms()))
		binary.BigEndian.PutUint64(out[8:], uint64(h.CID)<<8+i)
		p.emit(Message{Flags: 9, Src: uint64(h.PID), Proto: ProtoProbe, Port: 1, Payload: out})
	}
}

// HostRVCID is the RendezVous connection id the service host announces in its station URL (nex) and its packets. A real host's is
// its NEX connection id; the service is no NEX client, so it takes one that no live connection has (NEX hands out small ids from 1).
const HostRVCID = 0xFFF0

// ProbeTo makes the host probe a joiner's public address: what a console host does when the NEX server relays the joiner's
// RequestProbeInitiationExt (the joiner waits for it).
func (h *Host) ProbeTo(a netip.AddrPort) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.peers[a]; p != nil {
		p.probe()
		return
	}
	h.peer(a) // a new peer is probed on creation
}

// Handle processes one datagram from a joiner.
func (h *Host) Handle(from netip.AddrPort, b []byte) {
	hd, _, msgs, err := h.cipher.Open(b)
	if err != nil {
		return
	}
	h.HandleMessages(from, hd, msgs)
}

// HandleMessages processes the messages of an opened packet.
func (h *Host) HandleMessages(from netip.AddrPort, hd Header, msgs []Message) {
	h.mu.Lock()
	p := h.peer(from)
	p.timer = hd.Timer
	for _, m := range msgs {
		p.message(m)
	}
	deliver := p.pending
	p.pending = nil
	h.mu.Unlock()
	// the game callbacks run without the lock: they send (Peer.Send locks)
	for _, msg := range deliver {
		if p.OnGame != nil {
			p.OnGame(msg)
		}
	}
}

func (p *Peer) message(m Message) {
	h := p.h
	switch m.Proto {
	case ProtoProbe:
		if m.Port == 1 && len(m.Payload) >= 16 {
			out := make([]byte, 16)
			binary.BigEndian.PutUint32(out, h.RVCID)
			binary.BigEndian.PutUint32(out[4:], uint32(h.ms()))
			copy(out[8:], m.Payload[8:16]) // the sender's token comes back
			p.emit(Message{Flags: 9, Src: uint64(h.PID), Proto: ProtoProbe, Port: 2, Payload: out})
		}
	case ProtoRTT:
		if len(m.Payload) >= 16 && m.Payload[3] == 0 {
			out := append([]byte(nil), m.Payload[:16]...)
			out[3] = 1
			p.emit(p.conn(ProtoRTT, m.Port, out))
		}
	case ProtoClock:
		if len(m.Payload) >= 16 {
			out := append([]byte(nil), m.Payload[:8]...)
			out = binary.BigEndian.AppendUint64(out, h.ms())
			p.emit(p.conn(ProtoClock, m.Port, out))
		}
	case ProtoStation:
		p.stationMsg(m.Payload)
	case ProtoSession:
		if m.Port == 0 {
			p.sessionMsg(m.Payload)
		} else {
			p.reliable(m)
		}
	case ProtoReliable:
		p.reliable(m)
	default:
		h.Log.Info("pia: message not handled", "peer", p.Addr.String(), "type", fmt.Sprintf("%#x", m.Proto), "port", m.Port, "len", len(m.Payload), "head", hex.EncodeToString(m.Payload[:min(len(m.Payload), 24)]))
	}
}

func (p *Peer) stationMsg(pl []byte) {
	h := p.h
	if len(pl) < 8 {
		return
	}
	switch pl[0] {
	case 1:
		r, ok := parseConnRequest(pl)
		if !ok || r.Inverse {
			return
		}
		p.req, p.haveReq = r, true
		h.Log.Info("pia: connection request", "peer", p.Addr.String(), "conn", r.ConnID, "version", r.Version, "pid", r.Public.PID, "dest_pid", r.DestPID, "public", r.Public.Addr.String(), "private", r.Private.Addr.String())
		p.emit(p.station(stationAck(r.AckID)))
		if p.invSent {
			return
		}
		p.invSent = true
		p.ourAck = binary.BigEndian.Uint32(h.nonce[:4]) ^ uint32(h.ms())
		inv := connRequest{ConnID: p.ourConn, Version: r.Version, Inverse: true, DestPID: r.Public.PID, DestCID: r.Public.CID, DestConn: r.ConnID,
			Public: h.pub(), Private: h.priv(), AckID: p.ourAck}
		p.emit(p.station(inv.marshal()))
	case 2:
		if !p.haveReq || len(pl) < 255 {
			return
		}
		p.emit(p.station(stationAck(binary.BigEndian.Uint32(pl[251:]))))
		if !p.respSent {
			h.Log.Info("pia: connection response", "peer", p.Addr.String())
			p.respSent = true
			p.emit(p.station(connResponse(p.req.Version, p.req.Public.PID, p.req.Public.CID, p.ourAck+1)))
		}
	}
}

// meshHeader is the eight bytes after the join response's station count that the capture's host sent (docs/provenance.md P7).
var meshHeader = []byte{0x08, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x02}

func station(pub, priv Loc, idx uint8) []byte {
	b := append(pub.marshal(), priv.marshal()...)
	b = append(b, idx)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func (p *Peer) stations() []byte {
	h := p.h
	b := station(h.pub(), h.priv(), 0)
	return append(b, station(p.req.Public, p.req.Private, 1)...)
}

func (p *Peer) sessionMsg(pl []byte) {
	h := p.h
	if len(pl) < 14 || pl[0] != 1 || !p.haveReq {
		return
	}
	ack := binary.BigEndian.Uint32(pl[10:])
	p.emit(p.station(stationAck(ack)))
	if p.connected {
		return
	}
	p.connected = true
	h.Log.Info("pia: mesh join", "peer", p.Addr.String())
	resp := append([]byte{2, 2, 0, 1, 1, 0, 2, 0}, meshHeader...)
	resp = append(resp, p.stations()...)
	resp = binary.BigEndian.AppendUint32(resp, p.ourAck+2)
	ping := make([]byte, 16)
	binary.BigEndian.PutUint64(ping[8:], h.ms())
	p.emit(p.conn(ProtoRTT, 0, ping), p.conn(ProtoSession, 0, resp))
	time.AfterFunc(50*time.Millisecond, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		mesh := append([]byte{0x20, 2, 0, 0, 0, 0, 0, 3, 1, 0, 2, 0}, p.stations()...)
		for len(mesh) < 588 {
			mesh = append(mesh, 0)
		}
		p.send(ProtoSession, 1, mesh)
	})
}

func (p *Peer) stream(proto, port uint8) *stream {
	k := [2]uint8{proto, port}
	s := p.streams[k]
	if s == nil {
		s = &stream{proto: proto, port: port, sendSeq: FirstSeq, recvNext: FirstSeq}
		p.streams[k] = s
	}
	return s
}

// send queues a reliable message (fragmented to segMax) and pumps the stream.
func (p *Peer) send(proto, port uint8, msg []byte) {
	s := p.stream(proto, port)
	for len(msg) > 0 || s == nil {
		n := min(len(msg), segMax)
		fl := uint16(1)
		if n == len(msg) {
			fl |= 2
		}
		s.queue = append(s.queue, segment{seq: s.sendSeq, flags: fl, data: append([]byte(nil), msg[:n]...)})
		s.sendSeq++
		msg = msg[n:]
		if len(msg) == 0 {
			break
		}
	}
	p.pump(s)
}

// Send is the exported form for the game channel.
func (p *Peer) Send(msg []byte) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	p.send(ProtoReliable, 0, msg)
}

func (p *Peer) pump(s *stream) {
	for len(s.queue) > 0 && len(s.flight) < window {
		sg := s.queue[0]
		s.queue = s.queue[1:]
		sg.sent = time.Now()
		s.flight = append(s.flight, sg)
		p.emitSeg(s, sg)
	}
}

func (p *Peer) emitSeg(s *stream, sg segment) {
	r := Reliable{Flags: sg.flags, Seq: sg.seq, Ack: s.recvNext, Payload: sg.data}
	p.emit(p.conn(s.proto, s.port, r.Marshal()))
}

// Close forgets a peer.
func (h *Host) Close(a netip.AddrPort) {
	h.mu.Lock()
	delete(h.peers, a)
	h.mu.Unlock()
}

// Tick resends overdue segments; the owner calls it every 100 ms.
func (h *Host) Tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.peers {
		if p.connected {
			p.keepalives()
		}
		for _, s := range p.streams {
			for i := range s.flight {
				if time.Since(s.flight[i].sent) > resendAge {
					s.flight[i].sent = time.Now()
					p.emitSeg(s, s.flight[i])
				}
			}
		}
	}
}

// keepalives sends the periodic station traffic the console host sent in the capture: an RTT request every 0.2 s for the first
// eight, then every 2 s, and a clock request every second (P3). Without them the joiner's PIA has nothing to measure.
func (p *Peer) keepalives() {
	h := p.h
	gap := 2 * time.Second
	if p.rtts < 8 {
		gap = 200 * time.Millisecond
	}
	if time.Since(p.lastRTT) >= gap {
		p.lastRTT = time.Now()
		p.rtts++
		ping := make([]byte, 16)
		binary.BigEndian.PutUint64(ping[8:], h.ms())
		p.emit(p.conn(ProtoRTT, 0, ping))
	}
	if time.Since(p.lastClock) >= time.Second {
		p.lastClock = time.Now()
		req := binary.BigEndian.AppendUint64(nil, h.ms())
		p.emit(p.conn(ProtoClock, 0, binary.BigEndian.AppendUint64(req, 0)))
	}
}

func (p *Peer) reliable(m Message) {
	r, ok := ParseReliable(m.Payload)
	if !ok || !p.haveReq {
		return
	}
	s := p.stream(m.Proto, m.Port)
	// the peer's ack frees what it has received
	keep := s.flight[:0]
	for _, sg := range s.flight {
		if int32(sg.seq-r.Ack) >= 0 {
			keep = append(keep, sg)
		}
	}
	s.flight = keep
	p.pump(s)
	if r.Flags&1 == 0 {
		return
	}
	if r.Seq == s.recvNext {
		if m.Proto == ProtoReliable && r.Seq == FirstSeq {
			p.h.Log.Info("pia: first game segment", "peer", p.Addr.String(), "len", len(r.Payload))
		}
		s.recvNext++
		s.asm = append(s.asm, r.Payload...)
		if r.Flags&2 != 0 {
			msg := s.asm
			s.asm = nil
			if m.Proto == ProtoReliable {
				p.pending = append(p.pending, msg)
			}
		}
	}
	p.emit(p.conn(m.Proto, m.Port, Reliable{Ack: s.recvNext}.Marshal()))
}
