package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openpak/minecraft/internal/directory"
	"github.com/openpak/minecraft/internal/ps3join"
)

func testManager(t *testing.T, srvURL string, run func(context.Context, ps3join.Config) (ps3join.Progress, error)) *Manager {
	return &Manager{Config: Config{Matching: srvURL, Key: "k", Comm: "NPWR05636_00", BindIP: "127.0.0.1", PortLo: 38000, PortHi: 38009, TraceDir: t.TempDir(),
		ReadyTimeout: 1500 * time.Millisecond, WiiUTimeout: 1500 * time.Millisecond,
		ResolveWiiU: func(context.Context, uint32) (string, error) { return "acct", nil },
		Game: func(id string) (directory.Game, bool) {
			switch id {
			case "ps3:room":
				return directory.Game{ID: id, Platform: "ps3", HostAcct: "host"}, true
			case "wiiu:5":
				return directory.Game{ID: id, Platform: "wiiu", HostAcct: "host"}, true
			}
			return directory.Game{}, false
		}, Run: run}}
}

// Only a game the directory holds and a PS3 hosts is bridged to a Wii U joiner; a refused join starts nothing.
func TestPrepareRefuses(t *testing.T) {
	m := testManager(t, "http://127.0.0.1:1", nil)
	for game, want := range map[string]string{"ps3:gone": "not in the session directory", "wiiu:5": "only PS3-hosted games"} {
		if err := m.Prepare(game, 1, 2); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", game, err)
		}
	}
	if len(m.sessions) != 0 {
		t.Fatalf("%d sessions started", len(m.sessions))
	}
}

// A PS3 leg that cannot get its identity ends the session (and the Wii U gets its disconnect), and a Wii U that never shows up ends one that is
// ready; neither waits forever.
func TestSessionsDoNotHang(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no np identity", 404) }))
	defer srv.Close()
	m := testManager(t, srv.URL, nil)
	if err := m.Prepare("ps3:room", 1, 2); err != nil {
		t.Fatal(err)
	}
	wait(t, "identity failure ends the session", func() bool { return m.sessions[key("ps3:room", 2)].Ended() })
	// a console that keeps sending after its session ended, or that nex never pushed, starts nothing
	if _, err := m.Attach("ps3:room", 1, 2, func([]byte) {}, func() {}); err == nil {
		t.Fatal("an ended session took a console")
	}
	if _, err := m.Attach("ps3:room", 1, 99, func([]byte) {}, func() {}); err == nil || len(m.sessions) != 1 {
		t.Fatalf("a console nex never pushed started a session: %v", err)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"online_id":"Me","country":"gb","domain":"gb.gb.np.playstation.net","ticket":"dGlja2V0","comm":"NPWR05636_00","passphrase":"` + strings.Repeat("ab", 128) + `"}`))
	}))
	defer srv2.Close()
	m = testManager(t, srv2.URL, func(ctx context.Context, cfg ps3join.Config) (ps3join.Progress, error) {
		<-ctx.Done() // a PS3 leg that never reaches the game
		return ps3join.Progress{}, nil
	})
	if err := m.Prepare("ps3:room", 1, 3); err != nil {
		t.Fatal(err)
	}
	s := m.sessions[key("ps3:room", 3)]
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && !s.Ended() {
		time.Sleep(50 * time.Millisecond)
	}
	if !s.Ended() {
		t.Fatal("a PS3 leg that never got into the game was not ended")
	}
}
