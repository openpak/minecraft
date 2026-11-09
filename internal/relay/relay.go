// Package relay owns the Wii U leg's UDP socket: nex registers a leg (gathering, host PID, session key) when a console joins the
// service's world; the first packet from a new address is matched to its leg by whether the leg's key opens it.
package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/pia5"
	"github.com/openpak/minecraft/internal/presence"
	"github.com/openpak/minecraft/internal/store"
)

// LegSpec registers a leg.
type LegSpec struct {
	Gathering  int64  `json:"gathering"`
	HostPID    uint32 `json:"host_pid"`
	JoinerPID  uint32 `json:"joiner_pid"`
	SessionKey string `json:"session_key"` // hex, never logged or stored
}

type leg struct {
	id     int64
	spec   LegSpec
	key    []byte
	cipher *pia5.Cipher
	host   *pia5.Host
	added  time.Time
	hostID [20]byte
	peers  map[netip.AddrPort]*lce.Session
}

// Relay is the socket and its legs.
type Relay struct {
	Conn         net.PacketConn
	Public       netip.AddrPort
	Store        *store.Store
	JavaAddr     string
	HostName     string
	ViewDistance int
	LegTTL       time.Duration
	Presence     *presence.Reporter // nil: players are not published to the account core

	mu    sync.Mutex
	legs  map[int64]*leg
	peers map[netip.AddrPort]*leg
	next  int64
}

// Register adds a leg and returns its id.
func (r *Relay) Register(ctx context.Context, s LegSpec) (int64, error) {
	key, err := hex.DecodeString(s.SessionKey)
	if err != nil || len(key) < 16 {
		return 0, fmt.Errorf("session_key: want hex of at least 16 bytes")
	}
	c, err := pia5.NewCipher(key, uint32(s.Gathering))
	if err != nil {
		return 0, err
	}
	// nex re-registers the world's leg every minute: the same gathering and key keeps the live leg (and its players)
	r.mu.Lock()
	for _, l := range r.legs {
		if l.spec.Gathering == s.Gathering && l.spec.HostPID == s.HostPID && string(l.key) == string(key) {
			r.mu.Unlock()
			return l.id, nil
		}
	}
	r.mu.Unlock()
	var id int64
	if r.Store != nil {
		if id, err = r.Store.CreateLeg(ctx, s.Gathering, int64(s.HostPID), int64(s.JoinerPID)); err != nil {
			return 0, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.legs == nil {
		r.legs, r.peers = map[int64]*leg{}, map[netip.AddrPort]*leg{}
	}
	if id == 0 {
		r.next++
		id = r.next
	}
	l := &leg{id: id, spec: s, key: key, cipher: c, added: time.Now(), peers: map[netip.AddrPort]*lce.Session{}}
	rand.Read(l.hostID[:])
	l.host = pia5.NewHost(c, r.Public, s.HostPID, func(to netip.AddrPort, h pia5.Header, n [8]byte, msgs []pia5.Message) {
		r.Conn.WriteTo(c.Seal(h, n, msgs), net.UDPAddrFromAddrPort(to))
	})
	l.host.NewPeer = func(p *pia5.Peer) { r.newPeer(l, p) }
	r.legs[id] = l
	slog.Info("leg registered", "leg", id, "gathering", s.Gathering, "host_pid", s.HostPID)
	return id, nil
}

func (r *Relay) newPeer(l *leg, p *pia5.Peer) {
	var sid int64
	cfg := lce.Config{JavaAddr: r.JavaAddr, HostName: r.HostName, HostID: l.hostID, ViewDistance: r.ViewDistance, Log: slog.Default().With("leg", l.id)}
	var leave func()
	cfg.Joined = func(string) {
		leave = r.Presence.Join(p.PID())
		if r.Store != nil {
			sid, _ = r.Store.StartSession(context.Background(), l.id, int64(p.PID()))
		}
	}
	cfg.Left = func(reason string, chunks int64) {
		if leave != nil {
			go leave()
		}
		if r.Store != nil && sid != 0 {
			r.Store.EndSession(context.Background(), sid, reason, chunks)
		}
		l.host.Close(p.Addr)
		r.mu.Lock()
		delete(l.peers, p.Addr)
		delete(r.peers, p.Addr)
		r.mu.Unlock()
	}
	l.peers[p.Addr] = lce.NewSession(cfg, p, l.spec.HostPID)
}

// ProbeJoiner has the leg of a gathering probe a joiner's public address (the joiner asked the NEX server to make the host do so).
func (r *Relay) ProbeJoiner(gathering int64, a netip.AddrPort) error {
	r.mu.Lock()
	var host *pia5.Host
	for _, l := range r.legs {
		if l.spec.Gathering == gathering {
			host = l.host
		}
	}
	r.mu.Unlock()
	if host == nil {
		return fmt.Errorf("no leg for gathering %d", gathering)
	}
	slog.Info("probing joiner", "gathering", gathering, "to", a.String())
	host.ProbeTo(a)
	return nil
}

// Legs lists the registered legs (no keys).
func (r *Relay) Legs() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, l := range r.legs {
		out = append(out, map[string]any{"id": l.id, "gathering": l.spec.Gathering, "host_pid": l.spec.HostPID, "peers": len(l.peers)})
	}
	return out
}

// Serve reads the socket until it closes.
func (r *Relay) Serve(ctx context.Context) error {
	go r.housekeeping(ctx)
	buf := make([]byte, 2048)
	for {
		n, from, err := r.Conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		b := append([]byte(nil), buf[:n]...)
		ap := from.(*net.UDPAddr).AddrPort()
		if l := r.legFor(b, ap); l != nil {
			l.host.Handle(ap, b)
		}
	}
}

func (r *Relay) legFor(b []byte, from netip.AddrPort) *leg {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l := r.peers[from]; l != nil {
		return l
	}
	for _, l := range r.legs {
		if _, _, _, err := l.cipher.Open(b); err == nil {
			r.peers[from] = l
			slog.Info("console connected", "leg", l.id, "peer", from.String())
			if r.Store != nil {
				go r.Store.Connected(context.Background(), l.id, from.String())
			}
			return l
		}
	}
	return nil
}

func (r *Relay) housekeeping(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.mu.Lock()
		legs := make([]*leg, 0, len(r.legs))
		for _, l := range r.legs {
			legs = append(legs, l)
		}
		if n++; n%300 == 0 { // every 30 s
			ttl := r.LegTTL
			if ttl == 0 {
				ttl = 5 * time.Minute
			}
			for id, l := range r.legs {
				if len(l.peers) == 0 && time.Since(l.added) > ttl {
					delete(r.legs, id)
					slog.Info("leg dropped, never connected", "leg", id)
				}
			}
		}
		r.mu.Unlock()
		for _, l := range legs {
			l.host.Tick()
		}
	}
}
