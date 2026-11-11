package directory

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePresence struct {
	mu   sync.Mutex
	rows map[string]string // session -> "account/title"
	n    int
}

func (f *fakePresence) Register(_ context.Context, account, title string, _ time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprint("s", f.n)
	if f.rows == nil {
		f.rows = map[string]string{}
	}
	f.rows[id] = account + "/" + title
	return id, nil
}
func (f *fakePresence) Expire(_ context.Context, s string) error {
	f.mu.Lock()
	delete(f.rows, s)
	f.mu.Unlock()
	return nil
}

// A real PS3 room (the golden session data) becomes a Wii U gathering owned by the host's Wii U PID and the host's presence minecraft/g<id>;
// a real Wii U gathering becomes a bridged PS3 room plus presence data for the host. Withdrawing takes both away.
func TestAdaptersTalkToTheServices(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var bridged map[string]any
	var ps3Room map[string]any
	rec := func(r *http.Request) { mu.Lock(); calls = append(calls, r.Method+" "+r.URL.Path); mu.Unlock() }

	nex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		if r.Header.Get("Authorization") != "Bearer nexkey" {
			http.Error(w, "no", 401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/internal/minecraft/gatherings":
			w.Write([]byte(fmt.Sprintf(`[{"gathering":5,"host_pid":1111,"participants":[1111,2222],"max_participants":8,"open":true,"application_buffer":%q}]`, wiiuTemplateHex)))
		case r.Method == "PUT":
			json.NewDecoder(r.Body).Decode(&bridged)
			w.Write([]byte(`{"gathering":42}`))
		default:
			w.Write([]byte(`ok`))
		}
	}))
	defer nex.Close()
	resolve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pid") {
			w.Write([]byte(`{"found":true,"account_id":"acct-wiiu-host"}`))
			return
		}
		w.Write([]byte(`{"found":true,"pid":3333}`))
	}))
	defer resolve.Close()
	ps3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		switch {
		case r.Method == "GET" && r.URL.Path == "/internal/bridge/rooms":
			w.Write([]byte(fmt.Sprintf(`[{"id":"room-1","room_id":281474976710657,"owner":"acct-ps3-host","max":8,"external":%q}]`, ps3GoldExternalHex)))
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/internal/bridge/rooms/"):
			json.NewDecoder(r.Body).Decode(&ps3Room)
			w.Write([]byte(`{"room_id":281474976710658}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer ps3srv.Close()
	pres := &fakePresence{}
	wiiu := &WiiU{Nex: nex.URL, NexKey: "nexkey", ResolveURL: resolve.URL, Presence: pres}
	ps3 := &PS3{Matching: ps3srv.URL, Community: ps3srv.URL, Key: "k", Title: "NPWR05636_00"}
	ctx := context.Background()

	// source halves
	wg, err := wiiu.Hosted(ctx)
	if err != nil || len(wg) != 1 || wg[0].Ref != "5" || wg[0].HostAcct != "acct-wiiu-host" || wg[0].Settings.HostName != "OpenPak" || wg[0].Players != 2 || wg[0].MaxPlayers != 8 {
		t.Fatalf("wiiu hosted: %+v %v", wg, err)
	}
	pg, err := ps3.Hosted(ctx)
	if err != nil || len(pg) != 1 || pg[0].Ref != "room-1" || pg[0].HostAcct != "acct-ps3-host" || pg[0].Settings.HostName != "PS3Test" || pg[0].Players != 1 {
		t.Fatalf("ps3 hosted: %+v %v", pg, err)
	}

	// sink halves
	pg[0].Platform, pg[0].ID = "ps3", "ps3:room-1"
	if err := wiiu.Advertise(ctx, pg[0]); err != nil {
		t.Fatal(err)
	}
	if bridged["host_pid"].(float64) != 3333 || bridged["game"] != "ps3:room-1" {
		t.Fatalf("nex PUT: %v", bridged)
	}
	if buf, _ := hex.DecodeString(bridged["application_buffer"].(string)); len(buf) != 212 || buf[1] != 0x81 || cstr(buf[wiiuName2:]) != "PS3Test" {
		t.Fatalf("application buffer %x", buf)
	}
	if len(pres.rows) != 1 || pres.rows["s1"] != "acct-ps3-host/g42" {
		t.Fatalf("presence: %v", pres.rows)
	}
	wg[0].Platform, wg[0].ID = "wiiu", "wiiu:5"
	if err := ps3.Advertise(ctx, wg[0]); err != nil {
		t.Fatal(err)
	}
	if ps3Room["host_account"] != "acct-wiiu-host" || ps3Room["max_slot"].(float64) != 8 {
		t.Fatalf("matching2 PUT: %v", ps3Room)
	}
	if ext, _ := hex.DecodeString(ps3Room["external"].(string)); len(ext) != 184 || ext[1] != 0x84 || ext[ps3Players] != 2 {
		t.Fatalf("session data %x", ext)
	}
	if err := wiiu.Withdraw(ctx, "ps3:room-1"); err != nil || len(pres.rows) != 0 {
		t.Fatalf("withdraw: %v rows=%v", err, pres.rows)
	}
	if err := ps3.Withdraw(ctx, "wiiu:5"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "DELETE /internal/bridge/presence/acct-wiiu-host"
	found := false
	for _, c := range calls {
		found = found || c == want
	}
	if !found {
		t.Fatalf("no %q in %v", want, calls)
	}
}
