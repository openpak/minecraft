// Package ps3m2 is a headless PS3 Matching2 client: the console's side of np-matching2's wire (32-byte header, truncated HMAC-SHA1 MAC,
// TLV elements; research/ps3/matching2-protocol.md §3-7, §33-35). Copied and trimmed from github.com/openpak/np-matching2 (packages
// wire and client, the code its own `testclient` runs against the live agent): that module is not a dependency here, and only the
// joiner's few calls are needed. Keep in step by hand.
package ps3m2

import (
	"bufio"
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	HeaderLen   = 32
	KindRequest = 1
	KindReply   = 2
	KindEvent   = 4

	CmdPing         = 0x1001 // GetServerInfo on the console: element 0x1d = {u16 server id, u8 status}
	CmdLogin        = 0x1002
	CmdCreateServer = 0x1209 // what the console sends before a room request (0x29 = server id); reply carries the room-server list
	CmdSearchRoom   = 0x1200
	CmdGetExtList   = 0x1208 // GetRoomDataExternalList
	CmdJoinRoom     = 0x3201
	CmdLeaveRoom    = 0x3202
	CmdGetMember    = 0x3207

	EvMemberJoined = 0x320c
	EvMemberData   = 0x320d
	EvRoomData     = 0x320e
	EvMemberLeft   = 0x320f
)

// Frame is one request, reply or event.
type Frame struct {
	Kind   byte
	Cmd    uint16
	Result byte
	ReqID  [4]byte
	MAC    [16]byte
	Body   []byte
	Raw    []byte
}

// ParseFrame reads one whole frame from b (nil frame for a lone keepalive byte).
func ParseFrame(b []byte) (*Frame, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errors.New("short")
	}
	if b[0] == 0 {
		return nil, b[1:], nil
	}
	if len(b) < HeaderLen || b[0] != 0x11 {
		return nil, b, errors.New("not a frame")
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n < HeaderLen || n > len(b) {
		return nil, b, errors.New("short")
	}
	f := &Frame{Kind: b[1], Cmd: binary.BigEndian.Uint16(b[4:6]), Result: b[6], Body: b[HeaderLen:n], Raw: b[:n]}
	copy(f.ReqID[:], b[8:12])
	copy(f.MAC[:], b[16:32])
	return f, b[n:], nil
}

// Mac is HMAC-SHA1(key, packet with the MAC field zeroed)[:16].
func Mac(key, pkt []byte) []byte {
	z := append([]byte(nil), pkt...)
	for i := 16; i < 32 && i < len(z); i++ {
		z[i] = 0
	}
	h := hmac.New(sha1.New, key)
	h.Write(z)
	return h.Sum(nil)[:16]
}

// Verify checks the frame MAC with key.
func (f *Frame) Verify(key []byte) bool { return hmac.Equal(Mac(key, f.Raw), f.MAC[:]) }

// TLV is one element: u16 type<<12|id, u16 length, value.
func TLV(id, typ uint16, val []byte) []byte {
	b := make([]byte, 4, 4+len(val))
	binary.BigEndian.PutUint16(b, typ<<12|id)
	binary.BigEndian.PutUint16(b[2:], uint16(len(val)))
	return append(b, val...)
}

// Elements returns the top-level TLVs of body by id (the last of a repeated id).
func Elements(body []byte) map[uint16][]byte {
	out := map[uint16][]byte{}
	for p := body; len(p) >= 4; {
		id := binary.BigEndian.Uint16(p) & 0xfff
		n := int(binary.BigEndian.Uint16(p[2:]))
		if 4+n > len(p) {
			break
		}
		out[id] = p[4 : 4+n]
		p = p[4+n:]
	}
	return out
}

func (f *Frame) Elements() map[uint16][]byte { return Elements(f.Body) }

// Request builds a request MAC'd with key (nil: zero MAC).
func Request(key []byte, cmd uint16, reqID [4]byte, elems ...[]byte) []byte {
	b := make([]byte, HeaderLen)
	b[0], b[1] = 0x11, KindRequest
	binary.BigEndian.PutUint16(b[4:], cmd)
	copy(b[8:12], reqID[:])
	for _, e := range elems {
		b = append(b, e...)
	}
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	if key != nil {
		copy(b[16:32], Mac(key, b))
	}
	return b
}

// NpID is element 0x0b: the online id (16, NUL padded) and the options <cc><cc>ps3 the firmware derives from the RT JID.
func NpID(onlineID, country string) []byte {
	b := make([]byte, 24)
	copy(b, onlineID)
	c := strings.ToLower(country)
	if len(c) != 2 {
		c = "us"
	}
	copy(b[16:], c+c+"ps3")
	return b
}

// RoomKeyFromToken is what the firmware does with the 0x24 token: AES-128-ECB decryption with the first 16 bytes of the session key.
func RoomKeyFromToken(sessionKey, token []byte) ([]byte, error) {
	if len(sessionKey) < 16 || len(token) == 0 || len(token)%16 != 0 {
		return nil, errors.New("bad room key token")
	}
	c, err := aes.NewCipher(sessionKey[:16])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(token))
	for i := 0; i < len(token); i += 16 {
		c.Decrypt(out[i:i+16], token[i:i+16])
	}
	return out, nil
}

// Client is one logged-in console: a plain connection (the requests, and later the room's events) and the TLS one that carried the login.
type Client struct {
	Comm string
	Pass []byte // the 128-byte title passphrase (never logged)
	Key  []byte // session key from the login (never logged)

	Plain, TLS net.Conn
	rd         *bufio.Reader
	seq        uint32
	wmu        sync.Mutex
}

// Dial opens both connections. serverName is the firmware's matching hostname (the TLS front routes on it).
func Dial(plainAddr, tlsAddr, serverName string) (*Client, error) {
	c := &Client{}
	var err error
	if c.Plain, err = net.DialTimeout("tcp", plainAddr, 10*time.Second); err != nil {
		return nil, err
	}
	tc, err := net.DialTimeout("tcp", tlsAddr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	c.TLS = tls.Client(tc, &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_256_CBC_SHA}})
	c.rd = bufio.NewReader(c.Plain)
	return c, nil
}

func (c *Client) id() [4]byte {
	c.seq++
	v := 0x10100000 + c.seq
	return [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// readFrame reads the next frame off conn through r, skipping lone keepalive bytes.
func readFrame(r *bufio.Reader) (*Frame, error) {
	for {
		b0, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b0 == 0 {
			continue
		}
		r.UnreadByte()
		hdr, err := r.Peek(4)
		if err != nil {
			return nil, err
		}
		n := int(binary.BigEndian.Uint16(hdr[2:]))
		if hdr[0] != 0x11 || n < HeaderLen {
			return nil, fmt.Errorf("bad frame header % x", hdr)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		f, _, err := ParseFrame(buf)
		return f, err
	}
}

func (c *Client) write(conn net.Conn, b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := conn.Write(b)
	return err
}

// roundTrip sends req on conn and reads one frame from rd; only valid while no reader goroutine runs (before Events).
func (c *Client) roundTrip(conn net.Conn, rd *bufio.Reader, req []byte) (*Frame, error) {
	if err := c.write(conn, req); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return readFrame(rd)
}

// Ping is 0x1001 on the plain connection, MAC'd with the passphrase (GetServerInfo).
func (c *Client) Ping() (*Frame, error) {
	c.write(c.Plain, []byte{0})
	f, err := c.roundTrip(c.Plain, c.rd, Request(c.Pass[:16], CmdPing, c.id(), TLV(0x10, 1, []byte(c.Comm)), TLV(0x73, 4, []byte{0, 1})))
	if err != nil {
		return nil, err
	}
	if !f.Verify(c.Pass[:16]) {
		return f, errors.New("ping reply MAC")
	}
	return f, nil
}

// Login is 0x1002 on the TLS connection; it stores the session key.
func (c *Client) Login(ticket []byte, onlineID, country string) (*Frame, error) {
	nm := make([]byte, 48)
	copy(nm, onlineID)
	req := Request(nil, CmdLogin, c.id(),
		TLV(0x0f, 1, []byte("np")), TLV(0x10, 1, []byte(c.Comm)), TLV(0x73, 4, []byte{0, 1}),
		TLV(0x0b, 0, NpID(onlineID, country)), TLV(0x0c, 1, nm), TLV(0x0d, 1, []byte("http://avatar.invalid/a.png")),
		TLV(0x15, 2, c.Pass), TLV(0x11, 2, ticket), TLV(0x6f, 2, make([]byte, 16)))
	f, err := c.roundTrip(c.TLS, bufio.NewReader(c.TLS), req)
	if err != nil {
		return nil, err
	}
	if f.Result != 0 {
		return f, errors.New("login refused")
	}
	c.Key = f.Elements()[0x22]
	return f, nil
}

// Call sends a request on the plain connection, MAC'd with the session key, and checks the reply MAC.
func (c *Client) Call(cmd uint16, elems ...[]byte) (*Frame, error) {
	f, err := c.roundTrip(c.Plain, c.rd, Request(c.Key, cmd, c.id(), elems...))
	if err != nil {
		return nil, err
	}
	if !f.Verify(c.Key) {
		return f, errors.New("reply MAC")
	}
	return f, nil
}

// Events reads the plain connection until it closes, sending the firmware's keepalive byte every 5 s (the agent drops a silent
// connection), and hands every frame (events, late replies) to fn. Call it after the last Call.
func (c *Client) Events(fn func(*Frame)) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c.write(c.Plain, []byte{0})
			}
		}
	}()
	for {
		c.Plain.SetReadDeadline(time.Time{})
		f, err := readFrame(c.rd)
		if err != nil {
			return err
		}
		fn(f)
	}
}

// Leave sends LeaveRoom (0x3202) without waiting for the reply (the Events reader takes it); call it last.
func (c *Client) Leave() error {
	return c.write(c.Plain, Request(c.Key, CmdLeaveRoom, c.id()))
}

// Close drops both connections.
func (c *Client) Close() {
	if c.Plain != nil {
		c.Plain.Close()
	}
	if c.TLS != nil {
		c.TLS.Close()
	}
}

// SearchItem is one room of a 0x1200 reply: the room id's low 48 bits as three u16, the owner's online id and the raw item.
type SearchItem struct {
	ID        [3]uint16
	Owner     string
	OwnerNpID []byte // 24 bytes: online id + opt, the console's own (np-matching2 >= v0.2.27 echoes its login 0x0b)
	Raw       []byte
}

// ParseSearch reads the 0x03 element of a SearchRoom reply: [u16 0x45][u16 count][u16 start][u16 total] then 0x45 items, each three u16,
// u32, u16, the owner's 24-byte NpId, then TLVs (np-matching2 wire.SearchItem).
func ParseSearch(el []byte) []SearchItem {
	if len(el) < 8 {
		return nil
	}
	var out []SearchItem
	for p := el[8:]; len(p) >= 4; {
		n := int(binary.BigEndian.Uint16(p[2:]))
		if 4+n > len(p) {
			break
		}
		v := p[4 : 4+n]
		p = p[4+n:]
		if len(v) < 12+24 {
			continue
		}
		it := SearchItem{Raw: v}
		for i := range it.ID {
			it.ID[i] = binary.BigEndian.Uint16(v[2*i:])
		}
		it.Owner = strings.TrimRight(string(v[12:12+16]), "\x00")
		it.OwnerNpID = v[12 : 12+24]
		out = append(out, it)
	}
	return out
}

// ParseExternalList reads the 0x02 element of a 0x1208 reply: [u16 0x45][u16 count] then 0x45 items in the SearchRoom layout; it returns
// the external bin attributes (0x55, 0x56) of each room, in order.
func ParseExternalList(el []byte) []map[uint16][]byte {
	if len(el) < 4 {
		return nil
	}
	var out []map[uint16][]byte
	for p := el[4:]; len(p) >= 4; {
		n := int(binary.BigEndian.Uint16(p[2:]))
		if 4+n > len(p) {
			break
		}
		v := p[4 : 4+n]
		p = p[4+n:]
		attrs := map[uint16][]byte{}
		if len(v) >= 12+24 {
			for id, val := range Elements(v[12+24:]) {
				attrs[id] = val
			}
		}
		out = append(out, attrs)
	}
	return out
}

// InternalBin extracts the data of an internal bin attr element (0x57/0x58): [u16 last-update member][8 bytes time][TLV 0x01 data].
func InternalBin(v []byte) (data []byte, lastMember uint16, ok bool) {
	if len(v) < 14 {
		return nil, 0, false
	}
	return Elements(v[10:])[0x01], binary.BigEndian.Uint16(v), true
}

// RoomAttrs returns the internal bins 0x57 and 0x58 of a room data element (0x71 of a join reply: a 0x20-byte head, then TLVs) or of
// the body of a 0x320e event (TLVs from the start).
func RoomAttrs(body []byte, head int) (a57, a58 []byte) {
	if len(body) < head {
		return nil, nil
	}
	el := Elements(body[head:])
	a57, _, _ = InternalBin(el[0x57])
	a58, _, _ = InternalBin(el[0x58])
	return
}

// Member is one 0x4b element of a member list: the wire id, the flag attr (owner bit 31) and the online id.
type Member struct {
	ID    uint16
	Flag  uint32
	Owner bool
	Name  string
	Bin59 []byte
}

// ParseMember reads a 0x4b value: u16 id, 8 bytes join date, u32 flag attr, nat, team, then TLVs 0x0b NpId, 0x0c name, 0x59 bin attr.
func ParseMember(v []byte) (Member, bool) {
	if len(v) < 16 {
		return Member{}, false
	}
	m := Member{ID: binary.BigEndian.Uint16(v), Flag: binary.BigEndian.Uint32(v[10:])}
	m.Owner = m.Flag&0x80000000 != 0
	el := Elements(v[16:])
	if n := el[0x0b]; len(n) >= 16 {
		m.Name = strings.TrimRight(string(n[:16]), "\x00")
	}
	if b := el[0x59]; len(b) >= 8 {
		m.Bin59 = Elements(b[8:])[0x01]
	}
	return m, true
}

// ParseMembers reads the 0x02 member list of a join reply: [u16 0x4b][u16 count] then 0x4b elements.
func ParseMembers(l []byte) []Member {
	if len(l) < 4 {
		return nil
	}
	var out []Member
	for p := l[4:]; len(p) >= 4; {
		n := int(binary.BigEndian.Uint16(p[2:]))
		if 4+n > len(p) {
			break
		}
		if m, ok := ParseMember(p[4 : 4+n]); ok {
			out = append(out, m)
		}
		p = p[4+n:]
	}
	return out
}
