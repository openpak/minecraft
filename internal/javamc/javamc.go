// Package javamc is a minimal Minecraft Java Edition 1.8.9 (protocol 47) client: handshake, offline-mode login, packet framing
// with compression, and the play packets the bridge reads. Layouts are from the public protocol documentation (minecraft.wiki).
package javamc

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"
)

const Protocol = 47

// Clientbound play packet ids (1.8).
const (
	KeepAlive     = 0x00
	JoinGame      = 0x01
	Chat          = 0x02
	TimeUpdate    = 0x03
	SpawnPosition = 0x05
	PosAndLook    = 0x08
	BlockChange   = 0x23
	MultiBlock    = 0x22
	ChunkData     = 0x21
	ChunkBulk     = 0x26
	Disconnect    = 0x40
)

// Client is one Java connection.
type Client struct {
	conn      net.Conn
	r         *bufio.Reader
	wmu       sync.Mutex
	threshold int // -1 until Set Compression
	Name      string
	UUID      string
}

// Dial connects, logs in as name (offline mode) and returns a client in the play state.
func Dial(addr, name string) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 1<<16), threshold: -1, Name: name}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	host, port, _ := net.SplitHostPort(addr)
	var p int
	fmt.Sscan(port, &p)
	hs := AppendVarInt(nil, Protocol)
	hs = AppendString(hs, host)
	hs = binary.BigEndian.AppendUint16(hs, uint16(p))
	hs = AppendVarInt(hs, 2)
	if err := c.Write(0x00, hs); err != nil {
		return nil, err
	}
	if err := c.Write(0x00, AppendString(nil, name)); err != nil {
		return nil, err
	}
	for {
		id, data, err := c.Read()
		if err != nil {
			conn.Close()
			return nil, err
		}
		switch id {
		case 0x00: // login disconnect
			conn.Close()
			return nil, fmt.Errorf("javamc: refused: %s", jsonText(data))
		case 0x03: // set compression
			t, _, _ := ReadVarInt(data)
			c.threshold = t
		case 0x02: // login success: uuid string, username string
			rd := &Reader{b: data}
			c.UUID, _ = rd.String()
			conn.SetDeadline(time.Time{})
			return c, nil
		}
	}
}

func jsonText(b []byte) string {
	s, _ := (&Reader{b: b}).String()
	return s
}

func (c *Client) Close() error { return c.conn.Close() }

// Write sends one packet.
func (c *Client) Write(id int, payload []byte) error {
	body := AppendVarInt(nil, id)
	body = append(body, payload...)
	var out []byte
	if c.threshold >= 0 {
		if len(body) >= c.threshold {
			var z bytes.Buffer
			zw := zlib.NewWriter(&z)
			zw.Write(body)
			zw.Close()
			inner := AppendVarInt(nil, len(body))
			inner = append(inner, z.Bytes()...)
			out = AppendVarInt(nil, len(inner))
			out = append(out, inner...)
		} else {
			out = AppendVarInt(nil, len(body)+1)
			out = append(out, 0)
			out = append(out, body...)
		}
	} else {
		out = AppendVarInt(nil, len(body))
		out = append(out, body...)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.conn.Write(out)
	return err
}

// Read returns the next packet's id and payload.
func (c *Client) Read() (int, []byte, error) {
	n, err := readVarInt(c.r)
	if err != nil {
		return 0, nil, err
	}
	if n < 0 || n > 1<<22 {
		return 0, nil, errors.New("javamc: bad packet length")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.r, b); err != nil {
		return 0, nil, err
	}
	if c.threshold >= 0 {
		dl, k, err := ReadVarInt(b)
		if err != nil {
			return 0, nil, err
		}
		b = b[k:]
		if dl != 0 {
			zr, err := zlib.NewReader(bytes.NewReader(b))
			if err != nil {
				return 0, nil, err
			}
			if b, err = io.ReadAll(io.LimitReader(zr, 1<<23)); err != nil {
				return 0, nil, err
			}
		}
	}
	id, k, err := ReadVarInt(b)
	if err != nil {
		return 0, nil, err
	}
	return id, b[k:], nil
}

// --- serverbound helpers

func (c *Client) KeepAlive(token int) error { return c.Write(KeepAlive, AppendVarInt(nil, token)) }

func (c *Client) Settings(viewDistance int) error {
	b := AppendString(nil, "en_GB")
	b = append(b, byte(viewDistance), 0, 1, 0x7f)
	return c.Write(0x15, b)
}

func (c *Client) Position(x, y, z float64, yaw, pitch float32, ground bool) error {
	b := binary.BigEndian.AppendUint64(nil, math.Float64bits(x))
	b = binary.BigEndian.AppendUint64(b, math.Float64bits(y))
	b = binary.BigEndian.AppendUint64(b, math.Float64bits(z))
	b = binary.BigEndian.AppendUint32(b, math.Float32bits(yaw))
	b = binary.BigEndian.AppendUint32(b, math.Float32bits(pitch))
	return c.Write(0x06, append(b, b2(ground)))
}

func (c *Client) Say(text string) error { return c.Write(0x01, AppendString(nil, text)) }

func b2(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// --- encoding

func AppendVarInt(b []byte, v int) []byte {
	u := uint32(v)
	for u >= 0x80 {
		b = append(b, byte(u)|0x80)
		u >>= 7
	}
	return append(b, byte(u))
}

func AppendString(b []byte, s string) []byte {
	b = AppendVarInt(b, len(s))
	return append(b, s...)
}

func readVarInt(r io.ByteReader) (int, error) {
	var v uint32
	for i := 0; i < 5; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint32(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return int(int32(v)), nil
		}
	}
	return 0, errors.New("javamc: varint too long")
}

func ReadVarInt(b []byte) (int, int, error) {
	r := bytes.NewReader(b)
	v, err := readVarInt(r)
	return v, len(b) - r.Len(), err
}

// Reader walks a payload.
type Reader struct {
	b   []byte
	err error
}

func NewReader(b []byte) *Reader { return &Reader{b: b} }

func (r *Reader) take(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = io.ErrUnexpectedEOF
		return make([]byte, max(n, 0))[:0:0]
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}
func (r *Reader) Err() error { return r.err }
func (r *Reader) Left() int  { return len(r.b) }
func (r *Reader) Byte() byte {
	if b := r.take(1); len(b) == 1 {
		return b[0]
	}
	return 0
}
func (r *Reader) Bool() bool { return r.Byte() != 0 }
func (r *Reader) Int() int32 {
	if b := r.take(4); len(b) == 4 {
		return int32(binary.BigEndian.Uint32(b))
	}
	return 0
}
func (r *Reader) UShort() uint16 {
	if b := r.take(2); len(b) == 2 {
		return binary.BigEndian.Uint16(b)
	}
	return 0
}
func (r *Reader) Long() int64 {
	if b := r.take(8); len(b) == 8 {
		return int64(binary.BigEndian.Uint64(b))
	}
	return 0
}
func (r *Reader) Float() float32  { return math.Float32frombits(uint32(r.Int())) }
func (r *Reader) Double() float64 { return math.Float64frombits(uint64(r.Long())) }
func (r *Reader) VarInt() int {
	v, k, err := ReadVarInt(r.b)
	if err != nil {
		r.err = err
		return 0
	}
	r.b = r.b[k:]
	return v
}
func (r *Reader) String() (string, error) {
	n := r.VarInt()
	b := r.take(n)
	return string(b), r.err
}
func (r *Reader) Bytes(n int) []byte { return r.take(n) }

// Position unpacks a block position (x 26 bits, y 12, z 26).
func (r *Reader) Position() (x, y, z int) {
	v := uint64(r.Long())
	x, y, z = int(int64(v)>>38), int(int64(v<<26)>>52), int(int64(v<<38)>>38)
	return
}

// Section is one 16x16x16 chunk section as Java sends it.
type Section struct {
	Y          int
	Blocks     [4096]uint16 // id<<4 | meta, index (y*16+z)*16+x
	BlockLight [2048]byte
	SkyLight   [2048]byte
}

// ChunkColumn is a Java chunk column.
type ChunkColumn struct {
	X, Z     int
	GroundUp bool
	Sections []Section
	Biomes   []byte // 256, z*16+x, only when GroundUp
}

// ParseColumn decodes the data of one chunk: the sections named by mask, blocks then block light then sky light.
func ParseColumn(x, z int, ground bool, mask uint16, sky bool, data []byte) (*ChunkColumn, int, error) {
	col := &ChunkColumn{X: x, Z: z, GroundUp: ground}
	n := 0
	for s := 0; s < 16; s++ {
		if mask>>s&1 == 1 {
			col.Sections = append(col.Sections, Section{Y: s})
			n++
		}
	}
	need := n*4096*2 + n*2048
	if sky {
		need += n * 2048
	}
	if ground {
		need += 256
	}
	if len(data) < need {
		return nil, 0, fmt.Errorf("javamc: chunk data %d bytes, need %d", len(data), need)
	}
	o := 0
	for i := range col.Sections {
		for j := range col.Sections[i].Blocks {
			col.Sections[i].Blocks[j] = binary.LittleEndian.Uint16(data[o:])
			o += 2
		}
	}
	for i := range col.Sections {
		copy(col.Sections[i].BlockLight[:], data[o:])
		o += 2048
	}
	if sky {
		for i := range col.Sections {
			copy(col.Sections[i].SkyLight[:], data[o:])
			o += 2048
		}
	}
	if ground {
		col.Biomes = append([]byte(nil), data[o:o+256]...)
		o += 256
	}
	return col, o, nil
}

// Columns decodes a Chunk Data (0x21) or Map Chunk Bulk (0x26) payload.
func Columns(id int, payload []byte, sky bool) ([]*ChunkColumn, error) {
	r := NewReader(payload)
	if id == ChunkData {
		x, z, ground := int(r.Int()), int(r.Int()), r.Bool()
		mask := r.UShort()
		size := r.VarInt()
		data := r.Bytes(size)
		if r.Err() != nil {
			return nil, r.Err()
		}
		if mask == 0 && len(data) == 0 {
			return []*ChunkColumn{{X: x, Z: z, GroundUp: ground}}, nil // unload
		}
		c, _, err := ParseColumn(x, z, ground, mask, sky, data)
		if err != nil {
			return nil, err
		}
		return []*ChunkColumn{c}, nil
	}
	sk := r.Bool()
	cnt := r.VarInt()
	type meta struct {
		x, z int
		mask uint16
	}
	ms := make([]meta, cnt)
	for i := range ms {
		ms[i] = meta{int(r.Int()), int(r.Int()), r.UShort()}
	}
	if r.Err() != nil {
		return nil, r.Err()
	}
	data := r.Bytes(r.Left())
	var out []*ChunkColumn
	for _, m := range ms {
		c, n, err := ParseColumn(m.x, m.z, true, m.mask, sk, data)
		if err != nil {
			return nil, err
		}
		data = data[n:]
		out = append(out, c)
	}
	return out, nil
}

func newBuf(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, 1<<16) }
