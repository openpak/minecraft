// Package walk splits a console (LCE) game byte stream into packets and translates packets between the Wii U and PS3
// dialects. The stream carries no packet lengths (`Packet::readPacket`, Wii U rpx 0x02703cc8: one id byte, then the packet's
// own read()), so every packet is parsed to find where it ends; one wrong layout desynchronises everything after it.
//
// Layouts are the Wii U v688 read() functions (Minecraft.Client.rpx symbols, docs/provenance.md W1-W9). The PS3 01.84 dialect
// is the same table with a 28-byte PlayerUID; it is binary-derived only (no PS3 capture exists yet), see Dialect.
package walk

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Dialect is a console build.
type Dialect uint8

const (
	// WiiU is Minecraft: Wii U Edition v688 (title 00050000101D7500), confirmed by real-console captures.
	WiiU Dialect = iota
	// PS3 is Minecraft: PlayStation 3 Edition NPEB01899 01.84. Binary-derived only: its read() sequences were compared with
	// the Wii U ones (docs/provenance.md W10); no PS3 game stream has been captured.
	PS3
)

func (d Dialect) String() string {
	if d == PS3 {
		return "ps3"
	}
	return "wiiu"
}

// NetVersion is the short each build writes first in E8 and 02 and its host requires exactly (Wii U handlePreLogin
// 0x03307d80, PS3 0xbc1778).
func (d Dialect) NetVersion() uint16 {
	if d == PS3 {
		return 0x0884
	}
	return 0x0881
}

// UIDLen is the PlayerUID size: DataInputStream::readPlayerUID reads 20 raw bytes on Wii U (0x0220c518), 28 on PS3 (stream
// slot 0x58, copied as seven words in ScoreboardPacket::RemovePlayerStatsData read 0x5828b8).
func (d Dialect) UIDLen() int {
	if d == PS3 {
		return 28
	}
	return 20
}

// Packet is one packet of the stream.
type Packet struct {
	ID  byte
	Raw []byte // id byte and body, aliasing the walked buffer
	// UIDs are the offsets in Raw of every PlayerUID field (02, 01, 14, D1, E7, CE).
	UIDs []int
	// Version is the offset in Raw of the net version short (E8, 02), 0 when the packet has none.
	Version int
}

var (
	// ErrShort means the buffer ends inside a packet: more bytes are needed.
	ErrShort = errors.New("walk: packet incomplete")
	// ErrUnknown means an id no layout covers; the stream cannot be walked past it.
	ErrUnknown = errors.New("walk: unknown packet id")
	// ErrBad means a field value no read() accepts (an entity data type above 11).
	ErrBad = errors.New("walk: malformed packet")
)

// Next parses the packet at the start of b.
func Next(b []byte, d Dialect) (Packet, error) {
	if len(b) == 0 {
		return Packet{}, ErrShort
	}
	f, ok := layouts[b[0]]
	if !ok {
		return Packet{ID: b[0]}, fmt.Errorf("%w %#02x", ErrUnknown, b[0])
	}
	r := &rd{b: b, i: 1, d: d}
	f(r)
	if r.err != nil {
		return Packet{ID: b[0]}, r.err
	}
	return Packet{ID: b[0], Raw: b[:r.i], UIDs: r.uids, Version: r.ver}, nil
}

// Walk splits b into whole packets. rest is the unparsed tail: a partial packet when err is nil, or the bytes from the
// failing packet on when err is ErrUnknown or ErrBad.
func Walk(b []byte, d Dialect) (pkts []Packet, rest []byte, err error) {
	for len(b) > 0 {
		p, err := Next(b, d)
		if errors.Is(err, ErrShort) {
			return pkts, b, nil
		}
		if err != nil {
			return pkts, b, err
		}
		pkts = append(pkts, p)
		b = b[len(p.Raw):]
	}
	return pkts, nil, nil
}

// rd reads one packet. Errors stick; every read after one is a no-op.
type rd struct {
	b    []byte
	i    int
	err  error
	d    Dialect
	uids []int
	ver  int
}

func (r *rd) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.i+n > len(r.b) {
		r.err = ErrShort
		return nil
	}
	s := r.b[r.i : r.i+n]
	r.i += n
	return s
}

func (r *rd) skip(n int) { r.take(n) }

func (r *rd) u8() int {
	if s := r.take(1); s != nil {
		return int(s[0])
	}
	return 0
}

func (r *rd) i16() int {
	if s := r.take(2); s != nil {
		return int(int16(binary.BigEndian.Uint16(s)))
	}
	return 0
}

func (r *rd) u16() int {
	if s := r.take(2); s != nil {
		return int(binary.BigEndian.Uint16(s))
	}
	return 0
}

func (r *rd) i32() int {
	if s := r.take(4); s != nil {
		return int(int32(binary.BigEndian.Uint32(s)))
	}
	return 0
}

func (r *rd) u32() uint32 {
	if s := r.take(4); s != nil {
		return binary.BigEndian.Uint32(s)
	}
	return 0
}

func (r *rd) u64() uint64 {
	if s := r.take(8); s != nil {
		return binary.BigEndian.Uint64(s)
	}
	return 0
}

// varint: readVarInt 0x02210d5c, 7 bits a byte, low first; gives up after the sixth byte.
func (r *rd) varint() int { return r.varN(6) }

// varlong: readVarLong, the same with up to ten bytes.
func (r *rd) varlong() { r.varN(10) }

func (r *rd) varN(max int) int {
	v := 0
	for k := 0; k < max; k++ {
		c := r.u8()
		v |= (c & 0x7f) << (7 * k)
		if c&0x80 == 0 || r.err != nil {
			break
		}
	}
	return v
}

// str: Packet::readUtf 0x02705834, short count then that many UTF-16BE units (none when the count is not positive).
func (r *rd) str() {
	if n := r.i16(); n > 0 {
		r.skip(2 * n)
	}
}

// jutf: DataInputStream::readUTF 0x0220c1fc, unsigned short byte length then modified UTF-8.
func (r *rd) jutf() { r.skip(r.u16()) }

func (r *rd) uid() {
	if r.err == nil {
		r.uids = append(r.uids, r.i)
	}
	r.skip(r.d.UIDLen())
}

// nbt: readNbt 0x02210ec4, short length, none when negative.
func (r *rd) nbt() {
	if n := r.i16(); n >= 0 {
		r.skip(n)
	}
}

// cnbt: readCompressedNbt 0x0221102c, short raw length then short compressed length (either negative: none) and the bytes.
func (r *rd) cnbt() {
	if r.i16() < 0 {
		return
	}
	if n := r.i16(); n >= 0 {
		r.skip(n)
	}
}

// item: readItem 0x0221126c, short id (negative: empty), byte count, short aux, nbt.
func (r *rd) item() {
	if r.i16() < 0 {
		return
	}
	r.skip(3)
	r.nbt()
}

// meta: SynchedEntityData::unpack 0x0299be04, entries of byte id (0xFF ends) and byte serializer type; type values from the
// twelve EntityDataSerializers read() functions (0x0239c9c4-0x023a7450).
func (r *rd) meta() {
	for r.err == nil {
		if r.u8() == 0xff {
			return
		}
		switch r.u8() {
		case 0, 10, 6: // byte, direction (one raw byte), boolean
			r.skip(1)
		case 1, 5: // int, block state: VarInt
			r.varint()
		case 2: // float
			r.skip(4)
		case 3: // string: readUTF
			r.jutf()
		case 4: // item
			r.item()
		case 7: // rotations: three floats
			r.skip(12)
		case 8: // block pos: long
			r.skip(8)
		case 9: // optional block pos
			if r.u8() != 0 {
				r.skip(8)
			}
		case 11: // compound tag
			r.nbt()
		default:
			if r.err == nil {
				r.err = fmt.Errorf("%w: entity data type", ErrBad)
			}
		}
	}
}

// flexible: FlexiblePacket::read 0x02401100. Header int: strings in bits 0-7, VarInts in 8-19, booleans in 20-27 (all in
// one byte), floats in 28-31.
func (r *rd) flexible() {
	h := r.u32()
	for k := 0; k < int(h&0xff); k++ {
		r.str()
	}
	for k := 0; k < int(h>>8&0xfff); k++ {
		r.varint()
	}
	if h>>20&0xff != 0 {
		r.skip(1)
	}
	r.skip(4 * int(h>>28))
}

// shortBytes: a short count then the bytes when 1 <= count <= 0x7ffe (CustomPayload, GameCommand, MapSelectInfo).
func (r *rd) shortBytes() {
	if n := r.i16(); n >= 1 && n <= 0x7ffe {
		r.skip(n)
	}
}

// particleParams: ParticleType::getParamCount, the fourth constructor argument (static initialisers 0x027acb80-0x027ae560).
// Every other type has none. An id the build does not know falls back to a zero-parameter default.
var particleParams = map[int]int{39: 2, 0x200000: 1, 40: 1, 48: 1}
