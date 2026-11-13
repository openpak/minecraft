package relay

import (
	"log/slog"

	"github.com/openpak/minecraft/internal/pia5"
)

// bridgePeer wires a Wii U console that joined a bridged game (a game a player hosts on another console) to the bridge: its PID is known
// from its first game message, so the session is looked up then, not when the packet that created the peer arrives.
func (r *Relay) bridgePeer(l *leg, p *pia5.Peer) {
	r.mu.Lock()
	l.peers[p.Addr] = nil // counts as a player for the leg's idle drop
	r.mu.Unlock()
	var handle func([]byte)
	var failed bool
	p.OnGame = func(msg []byte) {
		if failed {
			return
		}
		if handle == nil {
			h, err := r.Bridge.Attach(l.spec.Game, l.spec.HostPID, p.PID(), p.Send, func() { r.dropPeer(l, p) })
			if err != nil {
				failed = true
				slog.Warn("bridged console refused", "leg", l.id, "game", l.spec.Game, "pid", p.PID(), "err", err)
				r.dropPeer(l, p)
				return
			}
			handle = h
		}
		handle(msg)
	}
}

// dropPeer forgets a console's PIA peer: its address may join again.
func (r *Relay) dropPeer(l *leg, p *pia5.Peer) {
	l.host.Close(p.Addr)
	r.mu.Lock()
	delete(l.peers, p.Addr)
	delete(r.peers, p.Addr)
	r.mu.Unlock()
}
