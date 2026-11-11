// Package directory is the platform-neutral session directory of the Minecraft cross-play bridge (card openpak/prds#177).
//
// One Game is one player-hosted game: a player hosts on their own console and the OpenPak service is only a bridge, never a host or a
// player. Each console network has an Adapter with two halves. Its source half reports the games hosted on that network's consoles; its
// sink half advertises a game hosted elsewhere through the network's own discovery, under the hosting player's identity there, keeps it
// in step and withdraws it. A join attempt by a local console on an advertised game comes back through Joins, so the transport leg
// (a separate piece) can answer it.
//
// A game hosted on ANY platform is advertised on EVERY other platform: Directory.Sync does that for whatever adapters it is given, so
// a new console network is one more Adapter and no change here. The platform-native <-> neutral mapping lives in the adapters' codecs
// (docs/session-mapping.md).
package directory

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Platform names a console network: "wiiu", "ps3"; later "x360", "switch", "vita", "ps4", "xboxone".
type Platform string

// State is whether a game accepts new players.
type State string

const (
	Open   State = "open"
	Closed State = "closed"
)

// Visibility is who may join, in the neutral form.
const (
	Friends         = "friends"
	FriendsOfFriend = "friends-of-friends"
)

// Settings is the world/game data every platform can express. Anything a platform carries beyond it is not translated (see the mapping
// document for which fields exist natively and how sure each mapping is).
type Settings struct {
	HostName   string // the host's name as the host's own console put it in the game data
	Visibility string // Friends or FriendsOfFriend
}

// Game is one player-hosted game.
type Game struct {
	ID         string   // directory id: "<platform>:<Ref>"
	Platform   Platform // the platform that hosts it
	Ref        string   // the platform-native session reference: a Wii U gathering id, a PS3 matchmaking session id
	HostAcct   string   // the hosting OpenPak account: every account has an identity on every network
	Settings   Settings
	Players    int
	MaxPlayers int
	State      State
}

// Who is a joining player: the id on the joiner's own network, and the OpenPak account when the adapter could resolve it.
type Who struct {
	Native  string // Wii U PID, PS3 online id
	Account string
}

// Join is one join attempt by a console of Platform on an advertised game, with whatever handshake data the console sent
// (station URLs, member data, probe data), verbatim and by name.
type Join struct {
	Time     time.Time
	Game     string // Game.ID
	Platform Platform
	Kind     string // "join", "probe", "signal"
	Who      Who
	Data     map[string]string
	Err      string // why the adapter refused it, "" when it was answered
}

// Adapter is one console network.
type Adapter interface {
	Platform() Platform
	// Hosted is every game hosted on this platform's consoles right now, bridged ones excluded.
	Hosted(ctx context.Context) ([]Game, error)
	// Advertise creates or updates the advertisement of a game hosted on another platform. It is idempotent: the directory calls it on
	// every change and again at least every Renew, which is also what keeps the advertisement alive.
	Advertise(ctx context.Context, g Game) error
	// Withdraw removes the advertisement of the game with this directory id.
	Withdraw(ctx context.Context, id string) error
	// Joins returns the join attempts seen since the previous call.
	Joins(ctx context.Context) ([]Join, error)
}

// Directory keeps the games of all platforms and advertises each on every other platform.
type Directory struct {
	Adapters []Adapter
	Renew    time.Duration // re-advertise unchanged games this often (default 30 s)
	Log      *slog.Logger
	Now      func() time.Time

	mu    sync.Mutex
	games map[string]*entry // by Game.ID
	joins []Join
}

type entry struct {
	game Game
	on   map[Platform]time.Time // platform -> when it last accepted the advertisement of this exact game
	adv  map[Platform]bool      // platforms that hold some advertisement of it (to withdraw)
}

const maxJoins = 200

func (d *Directory) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *Directory) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Sync is one pass: read every source, advertise new and changed games on every other platform, withdraw the games that are gone, collect
// the join attempts. A source that fails keeps its games as they were (a failed read is not "everybody left").
func (d *Directory) Sync(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.games == nil {
		d.games = map[string]*entry{}
	}
	renew := d.Renew
	if renew <= 0 {
		renew = 30 * time.Second
	}
	for _, src := range d.Adapters {
		hosted, err := src.Hosted(ctx)
		if err != nil {
			d.log().Warn("directory: source failed", "platform", src.Platform(), "err", err)
			continue
		}
		seen := map[string]bool{}
		for _, g := range hosted {
			g.Platform = src.Platform()
			if g.ID == "" {
				g.ID = string(g.Platform) + ":" + g.Ref
			}
			if g.HostAcct == "" {
				d.log().Warn("directory: game without a host account is not advertised", "game", g.ID)
				continue
			}
			seen[g.ID] = true
			e := d.games[g.ID]
			if e == nil {
				e = &entry{on: map[Platform]time.Time{}, adv: map[Platform]bool{}}
				d.games[g.ID] = e
				d.log().Info("directory: game appeared", "game", g.ID, "platform", g.Platform, "players", g.Players, "max", g.MaxPlayers)
			}
			if e.game != g {
				e.on = map[Platform]time.Time{} // changed: every sink hears it again
			}
			e.game = g
		}
		for id, e := range d.games {
			if e.game.Platform == src.Platform() && !seen[id] {
				d.withdraw(ctx, id, e)
				delete(d.games, id)
			}
		}
	}
	for id, e := range d.games {
		for _, sink := range d.Adapters {
			p := sink.Platform()
			if p == e.game.Platform {
				continue
			}
			if at, ok := e.on[p]; ok && d.now().Sub(at) < renew {
				continue
			}
			if err := sink.Advertise(ctx, e.game); err != nil {
				d.log().Warn("directory: advertise failed", "game", id, "on", p, "err", err)
				delete(e.on, p)
				continue
			}
			e.on[p], e.adv[p] = d.now(), true
		}
	}
	for _, a := range d.Adapters {
		js, err := a.Joins(ctx)
		if err != nil {
			d.log().Warn("directory: joins failed", "platform", a.Platform(), "err", err)
			continue
		}
		for _, j := range js {
			d.log().Info("directory: join", "game", j.Game, "platform", j.Platform, "kind", j.Kind, "who", j.Who.Native, "account", j.Who.Account, "err", j.Err, "data", j.Data)
			d.joins = append(d.joins, j)
		}
		if over := len(d.joins) - maxJoins; over > 0 {
			d.joins = d.joins[over:]
		}
	}
}

func (d *Directory) withdraw(ctx context.Context, id string, e *entry) {
	d.log().Info("directory: game gone", "game", id)
	for _, sink := range d.Adapters {
		p := sink.Platform()
		if p == e.game.Platform {
			continue
		}
		if !e.adv[p] {
			continue // never advertised there: nothing to take back
		}
		if err := sink.Withdraw(ctx, id); err != nil {
			d.log().Warn("directory: withdraw failed", "game", id, "on", p, "err", err)
		}
	}
}

// Games is a copy of the current directory, ordered by id.
func (d *Directory) Games() []Game {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Game, 0, len(d.games))
	for _, e := range d.games {
		out = append(out, e.game)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RecentJoins is the last join attempts (at most 200), oldest first.
func (d *Directory) RecentJoins() []Join {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Join(nil), d.joins...)
}

// Run syncs every interval until ctx ends.
func (d *Directory) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		d.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
