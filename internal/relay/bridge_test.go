package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openpak/minecraft/internal/bridge"
	"github.com/openpak/minecraft/internal/directory"
	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/lce/walk"
	"github.com/openpak/minecraft/internal/pia5"
	"github.com/openpak/minecraft/internal/ps3join"
)

// What a real PS3 host (PS3Test) sent and received in mc5 (2026-11-13, scratch/ps3-signaling-2026-11-09/mc5.{rx,tx}.bin): the E8 a PS3
// joiner "SwitchTest" sends, the host's first message (00 KeepAlive + 02 ClientboundPreLogin with its PlayerUID) and its FF Disconnect.
const (
	mc5Prelog  = "e80884000a0053007700690074006300680054006500730074"
	mc5Reply   = "00000000000208840001002d000000000001505333546573740000000000000000000080001fa732aa1a00000003000000000000000000000000000000003e9d0000000000000000000f"
	mc5Dropped = "ff00000003"
)

func unhex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// TestBridgedJoinEndToEnd runs a Wii U console's join of a PS3-hosted game through everything but the network of the PS3: a PIA 5 joiner built
// from the capture's console (pia5.Joiner + lce.Client: FE hello, E8, 01), the relay's bridged leg with the gathering's session key, the manager
// (identity from a fake np-matching2, the joiner's own PS3 identity), the session's translation, and a fake PS3 host that answers with mc5's
// recorded bytes. It asserts what each side received for the login exchange, and the trace files.
func TestBridgedJoinEndToEnd(t *testing.T) {
	// np-matching2: the identity route, as the real one answers
	nm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" || r.URL.Path != "/internal/bridge/identity" {
			http.Error(w, "no", 401)
			return
		}
		out := map[string]string{"online_id": "SwitchTest", "country": "gb", "domain": "gb.gb.np.playstation.net"}
		if r.URL.Query().Get("account") == "acct-host" {
			out["online_id"] = "PS3Test"
		}
		if r.URL.Query().Get("ticket") == "1" {
			out["ticket"] = base64.StdEncoding.EncodeToString([]byte("a-ticket"))
			out["comm"], out["passphrase"] = "NPWR05636_00", strings.Repeat("ab", 128)
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer nm.Close()

	const hostPID, joinerPID, gathering = 1630024257, 1570020199, 12
	var mu sync.Mutex
	var hostGot [][]byte // what the PS3 host received (translated)
	var cfgSeen ps3join.Config
	ps3Done := make(chan struct{})
	fakePS3 := func(ctx context.Context, cfg ps3join.Config) (ps3join.Progress, error) {
		cfgSeen = cfg
		uid := ps3join.PlayerUID(cfg.OnlineID, 0, [6]byte{0, 0x1f, 0xa7, 0x0f, 0, 1}, 1)
		host := ps3join.PlayerUID("PS3Test", 0, [6]byte{0, 0x1f, 0xa7, 0x32, 0xaa, 0x1a}, 3)
		var layer ps3join.GameLayer
		env := ps3join.GameEnv{Name: cfg.OnlineID, UID: uid, Host: host, Small: 1, Logf: cfg.Rec.Logf, Leave: func(string) {}}
		env.Write = func(b []byte) {
			mu.Lock()
			hostGot = append(hostGot, bytes.Clone(b))
			mu.Unlock()
			cfg.Rec.Game("tx", b)
			switch b[0] {
			case 0xE8: // the host answers the pre-login as mc5's did
				go layer.OnGame(unhex(mc5Reply))
			case 0x01: // and drops the player after the login, as mc5's did after 30 s
				go layer.OnGame(unhex(mc5Dropped))
			}
		}
		layer = cfg.NewGame(env)
		layer.Start()
		<-ctx.Done() // the leg holds until the session cancels it
		close(ps3Done)
		return ps3join.Progress{}, nil
	}
	games := func(id string) (directory.Game, bool) {
		return directory.Game{ID: id, Platform: "ps3", HostAcct: "acct-host", Settings: directory.Settings{HostName: "PS3Test"}}, id == "ps3:e8ca9e01df86"
	}
	traces := t.TempDir()
	port := freeUDP(t)
	mgr := &bridge.Manager{Config: bridge.Config{Matching: nm.URL, Key: "k", Comm: "NPWR05636_00", BindIP: "127.0.0.1", PublicIP: "127.0.0.1", PortLo: port, PortHi: port,
		TraceDir: traces, ResolveWiiU: func(_ context.Context, pid uint32) (string, error) { return "acct-joiner", nil }, Game: games, Run: fakePS3}}

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	srv := netip.MustParseAddrPort(conn.LocalAddr().String())
	r := &Relay{Conn: conn, Public: srv, Bridge: mgr}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx)
	key := bytes.Repeat([]byte{7}, 32)
	spec := LegSpec{Gathering: gathering, HostPID: hostPID, JoinerPID: joinerPID, SessionKey: hex.EncodeToString(key), Game: "ps3:e8ca9e01df86"}
	if _, err := r.Register(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// a leg for a game that is not in the directory is refused (nex logs it), as is a bridged leg without a joiner
	bad := spec
	bad.Game, bad.SessionKey = "ps3:gone", hex.EncodeToString(bytes.Repeat([]byte{8}, 32))
	if _, err := r.Register(ctx, bad); err == nil {
		t.Fatal("a game the directory does not hold was registered")
	}

	// the Wii U: PIA 5 joiner with the capture's console messages
	cipher, _ := pia5.NewCipher(key, gathering)
	jc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer jc.Close()
	self := netip.MustParseAddrPort(jc.LocalAddr().String())
	j := &pia5.Joiner{Conn: jc, Host: srv, Cipher: cipher, PID: joinerPID, HostPID: hostPID, Self: self, Joined: make(chan struct{})}
	cl := &lce.Client{Name: "WiiUPlayer", PID: joinerPID, HostPID: hostPID, Send: j.SendGame, Log: nil}
	j.OnGame = cl.OnGame
	go j.Run(ctx)
	select {
	case <-j.Joined:
	case <-time.After(15 * time.Second):
		t.Fatal("the Wii U's PIA session did not come up")
	}
	cl.Start()
	defer cl.Stop()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && cl.Stats().Disconnect == "" {
		time.Sleep(50 * time.Millisecond)
	}
	st := cl.Stats()
	if !st.HostHello || st.HostName != "PS3Test" || !st.PreLogin || st.Disconnect != "0x3" {
		t.Fatalf("the Wii U saw %+v", st)
	}
	select {
	case <-ps3Done:
	case <-time.After(5 * time.Second):
		t.Fatal("the PS3 leg was not cancelled when the host dropped the player")
	}
	// the PS3 leg joined as the JOINING player: its identity, ticket, host, domain and a port of the range
	if cfgSeen.OnlineID != "SwitchTest" || cfgSeen.HostID != "PS3Test" || cfgSeen.Domain != "gb.gb.np.playstation.net" || string(cfgSeen.Ticket) != "a-ticket" ||
		len(cfgSeen.Pass) != 128 || cfgSeen.Port != port || cfgSeen.PublicIP != "127.0.0.1" {
		t.Fatalf("PS3 leg config: online %q host %q domain %q port %d", cfgSeen.OnlineID, cfgSeen.HostID, cfgSeen.Domain, cfgSeen.Port)
	}
	// what the PS3 host received: mc5's E8 byte for byte, then the 01 with the PS3 identity in both id fields
	mu.Lock()
	defer mu.Unlock()
	if len(hostGot) < 2 || hex.EncodeToString(hostGot[0]) != mc5Prelog {
		t.Fatalf("PS3 host got %x", hostGot)
	}
	p, err := walk.Next(hostGot[1], walk.PS3)
	uid := ps3join.PlayerUID("SwitchTest", 0, [6]byte{0, 0x1f, 0xa7, 0x0f, 0, 1}, 1)
	if err != nil || p.ID != 1 || len(p.UIDs) != 2 || !bytes.Equal(hostGot[1][p.UIDs[0]:p.UIDs[0]+28], uid[:]) || !bytes.Equal(hostGot[1][p.UIDs[1]:p.UIDs[1]+28], uid[:]) {
		t.Fatalf("PS3 host's 01: %v %x", err, hostGot[1])
	}
	// the trace: the Wii U's messages, the PS3 side's bytes and the named packets of both directions
	files, _ := filepath.Glob(filepath.Join(traces, "*"))
	var log string
	sizes := map[string]int{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		sizes[strings.SplitN(filepath.Base(f), "-pid1570020199", 2)[1]] = len(b)
		if strings.HasSuffix(f, ".log") {
			log = string(b)
		}
	}
	for _, want := range []string{"STATE", "wiiu->ps3 0xe8", "ps3->wiiu 0x02", "wiiu->ps3 0x01", "ps3->wiiu 0xff"} {
		if !strings.Contains(strings.ToLower(log), strings.ToLower(want)) {
			t.Errorf("trace log has no %q:\n%s", want, log)
		}
	}
	if len(files) != 5 || sizes[".wiiu.rx.bin"] == 0 || sizes[".wiiu.tx.bin"] == 0 || sizes[".tx.bin"] == 0 || sizes[".log"] == 0 {
		t.Errorf("trace files %v sizes %v", files, sizes)
	}
}

func freeUDP(t *testing.T) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}
