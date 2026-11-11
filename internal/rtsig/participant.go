package rtsig

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// Participant is the relay side of the udpp2p activation with one host (spec §7 of the decode).
// It is driven by parsed stanzas from the host and by ProbeOK once the UDP probe phase has seen a
// valid peer probe; every stanza it wants sent goes through Send. Mutual is set when both activated
// stanzas have been exchanged; OnMutual (if set) fires once at that moment.
type Participant struct {
	Self Candidate // the relay endpoint offered to the host (also the RUDP listen endpoint)

	// OfferCandidates, if non-empty, is the candidate list sent in the offer instead of just Self.
	// Mirror the host: offer stun (public mapped), static and local forms so its candidate selection
	// finds a pair it will probe.
	OfferCandidates []Candidate
	SID             uint16
	SKey            []byte // 16 bytes, stable for the session
	HostJID         string
	SelfJID         string
	Send            func(string) error
	OnMutual        func(host Candidate)
	OnProbe         func(host Candidate) // probing should start toward this candidate

	peerSID   uint16
	peerKey   []byte
	hostCand  Candidate
	hostCands []Candidate
	offerOut  bool // own offer sent
	offAck    bool // host acked own offer
	peerOff   bool // host offer received
	accOut    bool
	accAck    bool
	peerAcc   bool
	actOut    bool
	actIn     bool
	Mutual    bool
	seq       int
	mu        sync.Mutex // Start, ProbeOK and Handle run on the RT, UDP and timer goroutines
}

func (p *Participant) nextID() string { p.seq++; return "sig" + strconv.Itoa(p.seq) }

func (p *Participant) send(s string) error {
	if p.Send == nil {
		return fmt.Errorf("rtsig: no transport")
	}
	return p.Send(s)
}

// Start sends the relay's offer (step 3; also correct before the host's offer arrives).
func (p *Participant) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.start()
}

func (p *Participant) start() error {
	if p.offerOut {
		return nil
	}
	p.offerOut = true
	cs := p.OfferCandidates
	if len(cs) == 0 {
		cs = []Candidate{p.Self}
	}
	return p.send(Offer(p.nextID(), p.HostJID, p.SelfJID, p.SID, p.peerSID, p.SKey, 1, cs))
}

// ProbeOK reports the UDP probe phase succeeded; the accept can go out.
func (p *Participant) ProbeOK() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accOut || !p.peerOff {
		return nil
	}
	p.accOut = true
	c := p.Self
	c.MAddress, c.MPort = c.Address, c.Port
	return p.send(Accept(p.nextID(), p.HostJID, p.SelfJID, p.SID, p.peerSID, p.SKey, []Candidate{c}))
}

// NpIDs returns our and the host's NpIds in probe form, built from the JIDs of this RT exchange exactly
// as the host's vsh builds them (its connection keys on the "to" JID of our offer and our stamped "from").
func (p *Participant) NpIDs() (own, peer []byte, err error) {
	if own, err = NpIDFromJID(p.SelfJID); err != nil {
		return nil, nil, err
	}
	peer, err = NpIDFromJID(p.HostJID)
	return own, peer, err
}

// ProbePacketFor builds the type-1 probe to send to the host.
func (p *Participant) ProbePacketFor() ([]byte, error) {
	own, peer, err := p.NpIDs()
	if err != nil {
		return nil, err
	}
	return ProbePacket(own, peer, p.SID, p.peerSID, uint64(time.Now().UnixMicro())), nil
}

// ProbeResponseFor answers a host type-1 probe received from src; nil if it is not one. The NpIds are
// the request's own, swapped: the host looks its connection up with exactly those bytes.
func (p *Participant) ProbeResponseFor(req []byte, src *net.UDPAddr) []byte {
	own, peer, err := p.NpIDs()
	if err != nil || !ProbeFrom(req, own, peer) {
		return nil
	}
	return ProbeResponsePacket(req, src, req[0x24:0x44], req[0x04:0x24], p.SID, p.peerSID)
}

// FromHost reports whether body is a genuine host probe (not our own, hairpinned back).
func (p *Participant) FromHost(body []byte) bool {
	own, peer, err := p.NpIDs()
	return err == nil && ProbeFrom(body, own, peer)
}

// ProbeTargets returns every candidate the host offered.
func (p *Participant) ProbeTargets() []Candidate { return p.hostCands }

func (p *Participant) maybeActivated() error {
	// ponytail: no wait for the host's ack of our accept: np-community (<= v0.4.8) drops the
	// firmware's bare results, so it never arrives. The RT stream keeps order, so the host still
	// sees accept before activated (event 0x21 in state 8/9 only sets its peer-activated flag).
	if p.actOut || !p.accOut || !p.peerAcc {
		return nil
	}
	p.actOut = true
	return p.send(Simple("activated", p.nextID(), p.HostJID, p.SelfJID, p.SID, p.peerSID))
}

func (p *Participant) checkMutual() {
	if p.Mutual || !p.actOut || !p.actIn {
		return
	}
	p.Mutual = true
	if p.OnMutual != nil {
		p.OnMutual(p.hostCand)
	}
}

// Handle consumes one parsed stanza from the host and drives the exchange.
func (p *Participant) Handle(st Stanza) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A result without an action acks one of our sets (bare from firmware, or with an empty
	// udpp2p query once relayed as our Ack is): offer first, then accept.
	if st.Kind == "result" && st.Action == "" && st.NS != NSNetinfo {
		if p.offerOut && !p.offAck {
			p.offAck = true
			return nil
		}
		if p.accOut && !p.accAck {
			p.accAck = true
			if err := p.maybeActivated(); err != nil {
				return err
			}
			p.checkMutual()
		}
		return nil
	}
	if st.NS == NSNetinfo {
		if st.Kind == "get" {
			return p.send(NetinfoResult(st.ID, p.HostJID, p.SelfJID, p.Self.Address, p.Self.Address, "unknown", "open", p.Self.Port, "type1"))
		}
		return nil
	}
	if st.NS != NSUdpp2p {
		return nil
	}
	switch st.Action {
	case "offer":
		if !p.offerOut {
			// Host-initiated (its game's ActivateConnection toward our Matching2 member NpId): adopt the
			// JIDs it used, so our offer, probes and the host's connection key on the same NpIds.
			if _, err := NpIDFromJID(st.From); err == nil {
				p.HostJID = st.From
			}
			if _, err := NpIDFromJID(st.To); err == nil {
				p.SelfJID = st.To
			}
		}
		p.peerSID, p.peerKey, p.peerOff = st.SID, st.SKey, true
		p.hostCands = st.Candidates
		if len(st.Candidates) > 0 {
			p.hostCand = st.Candidates[0]
			for _, c := range st.Candidates {
				if c.Type == "stun" {
					p.hostCand = c
				}
			}
		}
		if err := p.send(Ack(st.ID, p.HostJID, p.SelfJID)); err != nil {
			return err
		}
		if err := p.start(); err != nil {
			return err
		}
		if p.OnProbe != nil {
			p.OnProbe(p.hostCand)
		}
		return nil
	case "accept":
		if st.PSID != p.SID {
			return fmt.Errorf("rtsig: accept psid %d, want own sid %d", st.PSID, p.SID)
		}
		p.peerAcc = true
		if len(st.Candidates) > 0 {
			c := st.Candidates[0]
			if c.MAddress != "" {
				c.Address, c.Port = c.MAddress, c.MPort
			}
			p.hostCand = c
		}
		if err := p.send(Ack(st.ID, p.HostJID, p.SelfJID)); err != nil {
			return err
		}
		return p.maybeActivated()
	case "activated":
		p.actIn = true
		if err := p.send(Ack(st.ID, p.HostJID, p.SelfJID)); err != nil {
			return err
		}
		p.checkMutual()
		return nil
	case "terminate", "fterminate":
		return p.send(Ack(st.ID, p.HostJID, p.SelfJID))
	}
	return nil
}

// PeerSKey is the skey the host sent in its offer or accept (the key a datagram TO the host is encrypted with); nil before its offer.
func (p *Participant) PeerSKey() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.peerKey...)
}

// HostCandidate is the host endpoint chosen so far (the stun candidate of its offer, or its accept's mapped address).
func (p *Participant) HostCandidate() Candidate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hostCand
}
