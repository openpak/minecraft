package directory

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
)

// The Wii U adapter (Minecraft: Wii U Edition, nex title minecraft-wiiu). Its source half reads the gatherings real Wii Us host
// (GET /internal/minecraft/gatherings on the nex health listener). Its sink half keeps a gathering owned by the hosting player's Wii U
// PID with no connection behind it (PUT/DELETE /internal/minecraft/bridged/{game}) and publishes the host to the account core as
// minecraft/g<gathering>, which nn-friends renders to the host's Wii U friends as a joinable game (their Join tab is friend presence).
// Join attempts and the probe data a Wii U sends come back from /internal/minecraft/joins.

// WiiUNetVersion is the Wii U v688 net version, at +0 of the application buffer.
const WiiUNetVersion = 0x0881

// Offsets in the 212-byte application buffer of a gathering. Proof per field: docs/session-mapping.md.
const (
	wiiuBufLen   = 212
	wiiuName1    = 0x02 // host name, first copy; 9 bytes before the next used byte at 0x0b (guess: the field is longer on a real host)
	wiiuName1Len = 9
	wiiuName2    = 0x17 // host name, second copy, NUL-padded
	wiiuName2Len = 17
	wiiuVis      = 0xc3 // 0x8f = friends of friends (nex gameSpecificCanJoinMatchmakeSession), 0x8d otherwise
	wiiuPlayers  = 0xca // 2 in the capture row (guess: player count)
)

const wiiuTemplateHex = "08814f70656e50616b000028302010a000001b283028274f70656e50616b00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000003c8d00000000000102010f00000000000000"

// WiiUCodec is the Wii U <-> neutral mapping of the application buffer.
type WiiUCodec struct{}

// Buffer is the application buffer of a gathering with these settings, built on the capture's host row (docs/provenance.md N1):
// everything the mapping does not know stays as a real host wrote it.
func (WiiUCodec) Buffer(s Settings, players int) []byte {
	b, _ := hex.DecodeString(wiiuTemplateHex)
	putName(b[wiiuName1:wiiuName1+wiiuName1Len], s.HostName)
	putName(b[wiiuName2:wiiuName2+wiiuName2Len], s.HostName)
	b[wiiuVis] = 0x8d
	if s.Visibility == FriendsOfFriend {
		b[wiiuVis] = 0x8f
	}
	b[wiiuPlayers] = byte(max(0, min(players, 255)))
	return b
}

// Settings reads the application buffer of a real host.
func (WiiUCodec) Settings(buf []byte) (s Settings, players int, ok bool) {
	if len(buf) != wiiuBufLen || binary.BigEndian.Uint16(buf) != WiiUNetVersion {
		return s, 0, false
	}
	s.HostName = cstr(buf[wiiuName2 : wiiuName2+wiiuName2Len])
	s.Visibility = Friends
	if buf[wiiuVis] == 0x8f {
		s.Visibility = FriendsOfFriend
	}
	return s, int(buf[wiiuPlayers]), true
}

// Presence publishes and withdraws the host's line in the account core (the live-session registry).
type Presence interface {
	Register(ctx context.Context, account, title string, lease time.Duration) (session string, err error)
	Expire(ctx context.Context, session string) error
}

// CorePresence is Presence over the account core's Sessions service.
type CorePresence struct{ Sessions accountv1.SessionsClient }

func (c CorePresence) Register(ctx context.Context, account, title string, lease time.Duration) (string, error) {
	r, err := c.Sessions.RegisterSession(ctx, &accountv1.RegisterSessionRequest{AccountId: account, Namespace: "minecraft", TitleId: title,
		EndpointRef: "minecraft:bridge:" + title, LeaseSeconds: int32(lease / time.Second), Client: "bridge"})
	if err != nil {
		return "", err
	}
	return r.GetSessionId(), nil
}

func (c CorePresence) Expire(ctx context.Context, session string) error {
	_, err := c.Sessions.ExpireSession(ctx, &accountv1.ExpireSessionRequest{SessionId: session})
	return err
}

// WiiU is the adapter.
type WiiU struct {
	Nex, NexKey string // nex minecraft-wiiu health listener and MINECRAFT_INTERNAL_KEY
	ResolveURL  string // nn-account's /internal/resolve base
	ResolveKey  string
	Presence    Presence
	Lease       time.Duration // presence lease (default 90 s: the directory renews every 30 s)
	HTTP        *http.Client

	mu     sync.Mutex
	pids   map[string]uint32 // account -> Wii U PID
	accts  map[uint32]string // Wii U PID -> account
	pres   map[string]string // game id -> core session of the host's presence
	joinAt int64
}

func (w *WiiU) Platform() Platform { return "wiiu" }

func (w *WiiU) resolvePID(ctx context.Context, pid uint32) (string, error) {
	w.mu.Lock()
	a, ok := w.accts[pid]
	w.mu.Unlock()
	if ok {
		return a, nil
	}
	var out struct {
		Found   bool   `json:"found"`
		Account string `json:"account_id"`
	}
	if err := callJSON(ctx, w.HTTP, "GET", fmt.Sprintf("%s/internal/resolve/pid?namespace=wiiu&pid=%d", w.ResolveURL, pid), "", nil, &out, "X-Internal-Key", w.ResolveKey); err != nil {
		return "", err
	}
	if !out.Found {
		return "", fmt.Errorf("no account behind Wii U PID %d", pid)
	}
	w.mu.Lock()
	if w.accts == nil {
		w.accts = map[uint32]string{}
	}
	w.accts[pid] = out.Account
	w.mu.Unlock()
	return out.Account, nil
}

func (w *WiiU) resolveAccount(ctx context.Context, account string) (uint32, error) {
	w.mu.Lock()
	p, ok := w.pids[account]
	w.mu.Unlock()
	if ok {
		return p, nil
	}
	var out struct {
		Found bool   `json:"found"`
		PID   uint32 `json:"pid"`
	}
	if err := callJSON(ctx, w.HTTP, "GET", w.ResolveURL+"/internal/resolve/account?namespace=wiiu&account="+url.QueryEscape(account), "", nil, &out, "X-Internal-Key", w.ResolveKey); err != nil {
		return 0, err
	}
	if !out.Found || out.PID == 0 {
		return 0, fmt.Errorf("account %s has no Wii U identity", account)
	}
	w.mu.Lock()
	if w.pids == nil {
		w.pids = map[string]uint32{}
	}
	w.pids[account] = out.PID
	w.mu.Unlock()
	return out.PID, nil
}

type wiiuGathering struct {
	Gathering    uint32   `json:"gathering"`
	HostPID      uint32   `json:"host_pid"`
	Participants []uint64 `json:"participants"`
	Max          int      `json:"max_participants"`
	Open         bool     `json:"open"`
	Buffer       string   `json:"application_buffer"` // hex
}

// Hosted lists the gatherings real Wii Us host (nex leaves its own bridged ones out).
func (w *WiiU) Hosted(ctx context.Context) ([]Game, error) {
	var gs []wiiuGathering
	if err := callJSON(ctx, w.HTTP, "GET", w.Nex+"/internal/minecraft/gatherings", w.NexKey, nil, &gs); err != nil {
		return nil, err
	}
	var out []Game
	for _, g := range gs {
		buf, err := hex.DecodeString(g.Buffer)
		if err != nil {
			continue
		}
		s, _, ok := WiiUCodec{}.Settings(buf)
		if !ok {
			continue
		}
		acct, err := w.resolvePID(ctx, g.HostPID)
		if err != nil {
			continue // a host with no account cannot be advertised under a name; try again next pass
		}
		st := Open
		if !g.Open {
			st = Closed
		}
		out = append(out, Game{Ref: strconv.FormatUint(uint64(g.Gathering), 10), HostAcct: acct, Settings: s, Players: len(g.Participants), MaxPlayers: g.Max, State: st})
	}
	return out, nil
}

// Advertise keeps a gathering owned by the host's Wii U PID and the host's friend-visible presence for a game hosted elsewhere.
func (w *WiiU) Advertise(ctx context.Context, g Game) error {
	pid, err := w.resolveAccount(ctx, g.HostAcct)
	if err != nil {
		return err
	}
	body := map[string]any{"game": g.ID, "host_pid": pid, "application_buffer": hex.EncodeToString(WiiUCodec{}.Buffer(g.Settings, g.Players)),
		"max_participants": g.MaxPlayers, "open": g.State == Open, "friends_of_friends": g.Settings.Visibility == FriendsOfFriend}
	var res struct {
		Gathering uint32 `json:"gathering"`
	}
	if err := callJSON(ctx, w.HTTP, "PUT", w.Nex+"/internal/minecraft/bridged/"+url.PathEscape(g.ID), w.NexKey, body, &res); err != nil {
		return err
	}
	if w.Presence == nil {
		return nil
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 90 * time.Second
	}
	sid, err := w.Presence.Register(ctx, g.HostAcct, "g"+strconv.FormatUint(uint64(res.Gathering), 10), lease)
	if err != nil {
		return fmt.Errorf("presence: %w", err)
	}
	w.mu.Lock()
	if w.pres == nil {
		w.pres = map[string]string{}
	}
	w.pres[g.ID] = sid
	w.mu.Unlock()
	return nil
}

func (w *WiiU) Withdraw(ctx context.Context, id string) error {
	err := callJSON(ctx, w.HTTP, "DELETE", w.Nex+"/internal/minecraft/bridged/"+url.PathEscape(id), w.NexKey, nil, nil)
	w.mu.Lock()
	sid := w.pres[id]
	delete(w.pres, id)
	w.mu.Unlock()
	if sid != "" && w.Presence != nil {
		if e := w.Presence.Expire(ctx, sid); err == nil {
			err = e
		}
	}
	return err
}

// Joins reads the join attempts and probe data Wii Us sent for bridged gatherings.
func (w *WiiU) Joins(ctx context.Context) ([]Join, error) {
	var feed []struct {
		feedJoin
		PID uint32 `json:"pid"`
	}
	if err := callJSON(ctx, w.HTTP, "GET", w.Nex+"/internal/minecraft/joins?after="+strconv.FormatInt(w.joinAt, 10), w.NexKey, nil, &feed); err != nil {
		return nil, err
	}
	var out []Join
	for _, j := range feed {
		w.joinAt = max(w.joinAt, j.Seq)
		acct, _ := w.resolvePID(ctx, j.PID)
		out = append(out, Join{Time: j.Time, Game: j.Game, Platform: "wiiu", Kind: j.Kind, Who: Who{Native: strconv.FormatUint(uint64(j.PID), 10), Account: acct}, Data: j.Data, Err: j.Err})
	}
	return out, nil
}
