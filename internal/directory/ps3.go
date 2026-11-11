package directory

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The PS3 adapter (Minecraft: PlayStation 3 Edition 01.84, NPWR05636_00). Its source half reads the rooms real PS3s host on np-matching2
// (GET /internal/bridge/rooms); its sink half keeps a "bridged room" there on behalf of the hosting player, who is not connected to the
// room server (PUT/DELETE /internal/bridge/rooms/{game}), and puts the host's game presence for PS3 friends on np-community
// (PUT/DELETE /internal/bridge/presence/{account}). Join attempts come back from np-matching2 (/internal/bridge/joins) and the signalling
// stanzas aimed at the offline host from np-community (/internal/bridge/signaling). The route bodies are JSON; byte fields are hex.

// PS3NetVersion is the PS3 01.84 net version written at +0 of the session data and checked by every PS3 host and friend filter.
const PS3NetVersion = 0x0884

// Offsets in the 184-byte session data (attributes 0x55 and 0x56, one after the other). Proof per field: docs/session-mapping.md.
const (
	ps3ExtLen     = 184
	ps3Name       = 0x14 // host name, NUL-padded, 17 bytes (to the next used byte, 0x25)
	ps3NameLen    = 17
	ps3Vis        = 0xa7 // low byte of the u16 at 0xa6 (0x3c9d in the capture); low nibble 0xf = friends of friends (guess, by the Wii U twin)
	ps3Joinable   = 0xad // 1 in the capture; the 01.84 room list keeps a room only when it is non-zero
	ps3Players    = 0xae // player count of the host's console
	ps3Nibbles    = 0xb0 // two nibbles, 0x1f in the capture
	ps3TableLen   = 128  // data of attribute 0x57 and of 0x58
	ps3PresentLen = 56   // the presence data blob (builder 0xc93cd4; friend filter requires size 0x38)
)

// PS3Codec is the PS3 <-> neutral mapping of the session data, the player table and the presence blob.
type PS3Codec struct{}

func goldExternal() []byte { b, _ := hex.DecodeString(ps3GoldExternalHex); return b }
func goldTable() []byte    { b, _ := hex.DecodeString(ps3GoldTable1Hex + ps3GoldTable2Hex); return b }

func putName(dst []byte, name string) {
	for i := range dst {
		dst[i] = 0
	}
	n := copy(dst, name)
	if n == len(dst) {
		dst[n-1] = 0 // keep a terminator
	}
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// External is the 184-byte session data of a game with these settings (attributes 0x55 and 0x56 joined), built on the captured host's
// bytes: everything the mapping does not know stays as a real host wrote it.
func (PS3Codec) External(s Settings, players int) []byte {
	b := goldExternal()
	putName(b[ps3Name:ps3Name+ps3NameLen], s.HostName)
	b[ps3Vis] = b[ps3Vis]&0xf0 | 0x0d
	if s.Visibility == FriendsOfFriend {
		b[ps3Vis] = b[ps3Vis]&0xf0 | 0x0f
	}
	b[ps3Joinable] = 1
	b[ps3Players] = byte(max(0, min(players, 255)))
	return b
}

// Settings reads the session data of a real host.
func (PS3Codec) Settings(ext []byte) (s Settings, players int, ok bool) {
	if len(ext) != ps3ExtLen || binary.BigEndian.Uint16(ext) != PS3NetVersion {
		return s, 0, false
	}
	s.HostName = cstr(ext[ps3Name : ps3Name+ps3NameLen])
	s.Visibility = Friends
	if ext[ps3Vis]&0x0f == 0x0f {
		s.Visibility = FriendsOfFriend
	}
	return s, int(ext[ps3Players]), true
}

// Table is the 256-byte internal player table (attributes 0x57 and 0x58, 128 bytes each) with the host as the only player. The per-player
// bytes at +0x13..+0x16 are a value the console makes up; here they come from the host's account, so each bridged host is told apart
// (guess: what the game does with them is not known).
func (PS3Codec) Table(s Settings, account string) []byte {
	b := goldTable()
	putName(b[:0x11], s.HostName)
	h := sha256.Sum256([]byte(account))
	copy(b[0x14:0x17], h[:3])
	return b
}

// Presence is the 56-byte presence data blob the game publishes with sceNpBasicSetPresenceDetails2 and filters friends by. Layout from
// the 01.84 builder (research ps3-0184-vs-wiiu §7), values from this game's own session data. Not seen on the wire yet: a real console's
// presence stanza is not captured (np-community v0.4.11 logs it on the next real hosting).
func (PS3Codec) Presence(s Settings, roomID uint64, ext []byte) []byte {
	b := make([]byte, ps3PresentLen)
	putName(b[:18], s.HostName)
	binary.BigEndian.PutUint64(b[0x18:], roomID)
	binary.BigEndian.PutUint32(b[0x20:], 1) // server id
	binary.BigEndian.PutUint16(b[0x28:], PS3NetVersion)
	b[0x2b] = 0 // 01.02 lists a friend only when this byte is 0
	b[0x2c] = ext[ps3Nibbles] >> 4
	b[0x2d] = ext[ps3Nibbles] & 0x0f
	copy(b[0x30:0x34], ext[0xb4:0xb8])
	return b
}

// PS3 is the adapter.
type PS3 struct {
	Matching, Community string // base URLs of np-matching2 and np-community internal listeners
	Key                 string // bearer NP_BRIDGE_KEY (both services)
	Title               string // communication id, NPWR05636_00
	HTTP                *http.Client

	mu     sync.Mutex
	hosts  map[string]string // game id -> host account, for the presence withdrawal
	joinAt int64             // cursors of the two join feeds
	sigAt  int64
}

func (p *PS3) Platform() Platform { return "ps3" }

type ps3Room struct {
	ID       string `json:"id"`
	RoomID   uint64 `json:"room_id"`
	Owner    string `json:"owner"` // OpenPak account
	Max      int    `json:"max"`
	External string `json:"external"` // hex
}

// Hosted lists the rooms real PS3s host.
func (p *PS3) Hosted(ctx context.Context) ([]Game, error) {
	var rooms []ps3Room
	if err := callJSON(ctx, p.HTTP, "GET", p.Matching+"/internal/bridge/rooms", p.Key, nil, &rooms); err != nil {
		return nil, err
	}
	var out []Game
	for _, r := range rooms {
		ext, err := hex.DecodeString(r.External)
		if err != nil {
			continue
		}
		s, players, ok := PS3Codec{}.Settings(ext)
		if !ok {
			continue // a room that is not this game's session data: not a Minecraft game
		}
		out = append(out, Game{Ref: r.ID, HostAcct: r.Owner, Settings: s, Players: players, MaxPlayers: r.Max, State: Open})
	}
	return out, nil
}

// Advertise keeps the bridged room and the host's presence for a game hosted elsewhere.
func (p *PS3) Advertise(ctx context.Context, g Game) error {
	ext := PS3Codec{}.External(g.Settings, g.Players)
	body := map[string]any{"host_account": g.HostAcct, "max_slot": g.MaxPlayers, "external": hex.EncodeToString(ext),
		"internal": hex.EncodeToString(PS3Codec{}.Table(g.Settings, g.HostAcct)), "title": p.Title}
	var res struct {
		RoomID uint64 `json:"room_id"`
	}
	if err := callJSON(ctx, p.HTTP, "PUT", p.Matching+"/internal/bridge/rooms/"+url.PathEscape(g.ID), p.Key, body, &res); err != nil {
		return err
	}
	pres := map[string]any{"game": g.ID, "title": p.Title, "data": hex.EncodeToString(PS3Codec{}.Presence(g.Settings, res.RoomID, ext))}
	if err := callJSON(ctx, p.HTTP, "PUT", p.Community+"/internal/bridge/presence/"+url.PathEscape(g.HostAcct), p.Key, pres, nil); err != nil {
		return fmt.Errorf("presence: %w", err)
	}
	p.mu.Lock()
	if p.hosts == nil {
		p.hosts = map[string]string{}
	}
	p.hosts[g.ID] = g.HostAcct
	p.mu.Unlock()
	return nil
}

func (p *PS3) Withdraw(ctx context.Context, id string) error {
	err := callJSON(ctx, p.HTTP, "DELETE", p.Matching+"/internal/bridge/rooms/"+url.PathEscape(id), p.Key, nil, nil)
	p.mu.Lock()
	acct := p.hosts[id]
	delete(p.hosts, id)
	p.mu.Unlock()
	if acct != "" {
		if e := callJSON(ctx, p.HTTP, "DELETE", p.Community+"/internal/bridge/presence/"+url.PathEscape(acct), p.Key, nil, nil); err == nil {
			err = e
		}
	}
	return err
}

type feedJoin struct {
	Seq      int64             `json:"seq"`
	Time     time.Time         `json:"time"`
	Game     string            `json:"game"`
	Kind     string            `json:"kind"`
	Native   string            `json:"native"`
	Account  string            `json:"account"`
	Data     map[string]string `json:"data"`
	Err      string            `json:"err"`
	Platform Platform          `json:"platform"`
}

// Joins reads both feeds: JoinRoom attempts from np-matching2 and the signalling toward an offline host from np-community.
func (p *PS3) Joins(ctx context.Context) ([]Join, error) {
	var out []Join
	var firstErr error
	for _, f := range []struct {
		base, path string
		cur        *int64
	}{{p.Matching, "/internal/bridge/joins", &p.joinAt}, {p.Community, "/internal/bridge/signaling", &p.sigAt}} {
		var feed []feedJoin
		if err := callJSON(ctx, p.HTTP, "GET", f.base+f.path+"?after="+strconv.FormatInt(*f.cur, 10), p.Key, nil, &feed); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, j := range feed {
			*f.cur = max(*f.cur, j.Seq)
			out = append(out, Join{Time: j.Time, Game: j.Game, Platform: "ps3", Kind: j.Kind, Who: Who{Native: j.Native, Account: j.Account}, Data: j.Data, Err: j.Err})
		}
	}
	return out, firstErr
}
