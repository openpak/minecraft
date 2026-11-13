package walk

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"sync"
)

// Translate rewrites a packet parsed in dialect from for a reader in dialect to. It changes exactly three things:
//
//   - the net version short of E8 and 02 becomes to.NetVersion() (0x0881 <-> 0x0884);
//   - every PlayerUID (02, 01, 14, D1, E7, CE) is replaced by its counterpart from m, which changes its length 20 <-> 28;
//   - the compressed column of a chunk packet 0x33 going PS3 -> Wii U is re-wrapped (chunkToWiiU).
//
// Every other byte is returned unchanged. PlayerCombat D5 is not touched: both builds read and write event 2 the same way.
// With from == to, Raw is returned as is.
func Translate(p Packet, from, to Dialect, m *UIDMap) []byte {
	if from == to {
		return p.Raw
	}
	if p.ID == 0x33 && from == PS3 {
		return chunkToWiiU(p.Raw)
	}
	if p.Version == 0 && len(p.UIDs) == 0 {
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

// chunkToWiiU re-wraps the column of a PS3 chunk packet. Both consoles send `33, full flag, int x*16, short 0, int z*16, 15 255 15, int
// dimension<<30 | data length, data, short 0`, and the data holds the same run-length column (lcechunk.Unpack parses a PS3 one unchanged),
// but the PS3 data is `u32 BE inflated length, raw deflate` (research ps3-0184-world-data) where the Wii U's is a zlib stream. A packet that
// does not convert (the Wii U direction, damage) is returned as it is. Translating Wii U -> PS3 is not needed: a console sends no chunks.
func chunkToWiiU(raw []byte) []byte {
	const hdr = 19
	if len(raw) < hdr+4 {
		return raw
	}
	w := binary.BigEndian.Uint32(raw[15:])
	n := int(w & 0x3fffffff)
	if n < 4 || hdr+n > len(raw) {
		return raw
	}
	d := raw[hdr : hdr+n]
	rle, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(d[4:])), 1<<22))
	if err != nil || len(rle) != int(binary.BigEndian.Uint32(d)) {
		return raw
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(rle)
	zw.Close()
	out := append(make([]byte, 0, hdr+z.Len()+len(raw)-hdr-n), raw[:15]...)
	out = binary.BigEndian.AppendUint32(out, w&^0x3fffffff|uint32(z.Len()))
	out = append(out, z.Bytes()...)
	return append(out, raw[hdr+n:]...)
}
