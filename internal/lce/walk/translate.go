package walk

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"sync"
)

// Translate rewrites a packet parsed in dialect from for a reader in dialect to. It changes exactly two things:
//
//   - the net version short of E8 and 02 becomes to.NetVersion() (0x0881 <-> 0x0884);
//   - every PlayerUID (02, 01, 14, D1, E7, CE) is replaced by its counterpart from m, which changes its length 20 <-> 28.
//
// Every other byte is returned unchanged. PlayerCombat D5 is not touched: both builds read and write event 2 the same way.
// With from == to, Raw is returned as is.
func Translate(p Packet, from, to Dialect, m *UIDMap) []byte {
	if from == to || (p.Version == 0 && len(p.UIDs) == 0) {
		return p.Raw
	}
	out := make([]byte, 0, len(p.Raw)+len(p.UIDs)*8)
	at := 0
	if p.Version != 0 {
		out = append(out, p.Raw[:p.Version]...)
		out = binary.BigEndian.AppendUint16(out, to.NetVersion())
		at = p.Version + 2
	}
	n := from.UIDLen()
	for _, o := range p.UIDs {
		out = append(out, p.Raw[at:o]...)
		out = append(out, m.Map(p.Raw[o:o+n], from, to)...)
		at = o + n
	}
	return append(out, p.Raw[at:]...)
}

// UIDMap pairs each player's Wii U PlayerUID (20 bytes) with a PS3 one (28 bytes), so a player keeps one id on each side
// for the whole session and an id that crosses the bridge comes back as the original.
//
// How a counterpart is made when an id is first seen:
//   - Wii U -> PS3: the 20 bytes followed by 8 zero bytes.
//   - PS3 -> Wii U: an id ending in 8 zero bytes loses them (the inverse of the rule above); any other id becomes its SHA-1,
//     which is 20 bytes.
//   - all-zero and all-0xFF ids (the empty slots of 01 Login) map to the same fill at the other length.
//
// Unknown until a PS3 capture exists: what a real PS3 id looks like (01.02 notes suggest SceNpOnlineId plus 8 bytes), whether
// a PS3 accepts a synthesised id for a Wii U player, and which fill the PS3 uses for an empty slot. The bridge can seed real
// pairs with Pair before the first packet. The map is in memory; persisting it across sessions is the caller's choice.
type UIDMap struct {
	mu  sync.Mutex
	w2p map[string][]byte
	p2w map[string][]byte
}

// Pair records that w (Wii U, 20 bytes) and p (PS3, 28 bytes) are the same player.
func (m *UIDMap) Pair(w, p []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pair(w, p)
}

func (m *UIDMap) pair(w, p []byte) {
	if m.w2p == nil {
		m.w2p, m.p2w = map[string][]byte{}, map[string][]byte{}
	}
	w, p = bytes.Clone(w), bytes.Clone(p)
	m.w2p[string(w)], m.p2w[string(p)] = p, w
}

// Map returns the counterpart of id (from.UIDLen() bytes) in dialect to.
func (m *UIDMap) Map(id []byte, from, to Dialect) []byte {
	if from == to {
		return id
	}
	if fill, ok := uniform(id); ok {
		return bytes.Repeat([]byte{fill}, to.UIDLen())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if from == WiiU {
		if p, ok := m.w2p[string(id)]; ok {
			return p
		}
		p := append(bytes.Clone(id), make([]byte, 8)...)
		m.pair(id, p)
		return p
	}
	if w, ok := m.p2w[string(id)]; ok {
		return w
	}
	var w []byte
	if bytes.Equal(id[20:], make([]byte, 8)) {
		w = bytes.Clone(id[:20])
	} else {
		h := sha1.Sum(id)
		w = h[:]
	}
	m.pair(w, id)
	return w
}

func uniform(b []byte) (byte, bool) {
	if len(b) == 0 || (b[0] != 0 && b[0] != 0xff) {
		return 0, false
	}
	for _, c := range b {
		if c != b[0] {
			return 0, false
		}
	}
	return b[0], true
}
