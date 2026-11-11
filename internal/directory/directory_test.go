package directory

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fake is a platform with a list of hosted games, the adverts it holds for games hosted elsewhere, and join attempts to report.
type fake struct {
	p       Platform
	hosted  []Game
	adverts map[string]Game
	ads     int // Advertise calls
	joins   []Join
	failGet error
}

func newFake(p Platform) *fake { return &fake{p: p, adverts: map[string]Game{}} }

func (f *fake) Platform() Platform { return f.p }
func (f *fake) Hosted(context.Context) ([]Game, error) {
	return f.hosted, f.failGet
}
func (f *fake) Advertise(_ context.Context, g Game) error { f.ads++; f.adverts[g.ID] = g; return nil }
func (f *fake) Withdraw(_ context.Context, id string) error {
	delete(f.adverts, id)
	return nil
}
func (f *fake) Joins(context.Context) ([]Join, error) {
	j := f.joins
	f.joins = nil
	return j, nil
}

// A game hosted on A shows on B under the host's account and settings, is not echoed back to A, follows a change, is not re-sent while
// unchanged and fresh, is withdrawn when the host goes, and survives a failed read of its source. The reverse direction is the same
// code: B's game shows on A.
func TestGameFromAIsAdvertisedOnBAndWithdrawnWhenTheHostGoes(t *testing.T) {
	a, b := newFake("wiiu"), newFake("ps3")
	now := time.Unix(1000, 0)
	d := &Directory{Adapters: []Adapter{a, b}, Renew: 30 * time.Second, Now: func() time.Time { return now }}
	ctx := context.Background()

	g := Game{Ref: "7", HostAcct: "acct-1", Settings: Settings{HostName: "Alex", Visibility: Friends}, Players: 1, MaxPlayers: 8, State: Open}
	a.hosted = []Game{g}
	d.Sync(ctx)
	got, ok := b.adverts["wiiu:7"]
	if !ok {
		t.Fatal("game hosted on A is not advertised on B")
	}
	if got.HostAcct != "acct-1" || got.Settings.HostName != "Alex" || got.Platform != "wiiu" || got.Players != 1 {
		t.Fatalf("advert changed on the way: %+v", got)
	}
	if len(a.adverts) != 0 {
		t.Fatalf("game echoed back to its own platform: %v", a.adverts)
	}

	d.Sync(ctx)
	if b.ads != 1 {
		t.Fatalf("unchanged game re-sent inside Renew: %d adverts", b.ads)
	}
	now = now.Add(31 * time.Second)
	d.Sync(ctx)
	if b.ads != 2 {
		t.Fatalf("not renewed after Renew: %d adverts", b.ads)
	}

	g.Players = 3
	a.hosted = []Game{g}
	d.Sync(ctx)
	if b.ads != 3 || b.adverts["wiiu:7"].Players != 3 {
		t.Fatalf("a change was not passed on: ads=%d %+v", b.ads, b.adverts["wiiu:7"])
	}

	// the other direction, in the same directory
	b.hosted = []Game{{Ref: "room-9", HostAcct: "acct-2", Settings: Settings{HostName: "Sam"}, Players: 2, MaxPlayers: 8, State: Open}}
	d.Sync(ctx)
	if a.adverts["ps3:room-9"].HostAcct != "acct-2" {
		t.Fatalf("game hosted on B is not advertised on A: %v", a.adverts)
	}

	a.failGet = errors.New("nex down")
	d.Sync(ctx)
	if _, ok := b.adverts["wiiu:7"]; !ok {
		t.Fatal("a failed read withdrew the game")
	}
	a.failGet = nil

	a.hosted = nil // the host left
	d.Sync(ctx)
	if _, ok := b.adverts["wiiu:7"]; ok {
		t.Fatal("game still advertised on B after its host went")
	}
	if _, ok := a.adverts["ps3:room-9"]; !ok {
		t.Fatal("an unrelated game was withdrawn")
	}
}

func TestJoinsAreCollectedAndAGameWithoutAHostIsNotAdvertised(t *testing.T) {
	a, b := newFake("wiiu"), newFake("ps3")
	d := &Directory{Adapters: []Adapter{a, b}}
	a.hosted = []Game{{Ref: "1", Settings: Settings{HostName: "x"}}} // no host account
	b.joins = []Join{{Game: "wiiu:1", Platform: "ps3", Kind: "join", Who: Who{Native: "Sam"}, Data: map[string]string{"member_bin": "00000001"}}}
	d.Sync(context.Background())
	if len(b.adverts) != 0 {
		t.Fatal("a game without a host account was advertised")
	}
	js := d.RecentJoins()
	if len(js) != 1 || js[0].Who.Native != "Sam" || js[0].Data["member_bin"] != "00000001" {
		t.Fatalf("join not collected: %+v", js)
	}
}
