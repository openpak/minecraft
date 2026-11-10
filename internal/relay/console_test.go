package relay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/pia5"
)

func TestStationAddr(t *testing.T) {
	// the real Wii U joiner's URL from the 2026-11-10 21:13:48 nex log
	a, ok := stationAddr("prudp:/address=37.228.205.125;port=5272;CID=1232220597;PID=1570020176;R=0;RVCID=2;Ra=0.0.0.0;Rp=0;Rsa=0.0.0.0;Rsp=0;natf=2;natm=1;probeinit=1;sid=15;stream=0;type=3")
	if !ok || a.String() != "37.228.205.125:5272" {
		t.Fatalf("%v %v", a, ok)
	}
	if _, ok := stationAddr("prudp:/address=0.0.0.0;port=0"); ok {
		t.Fatal("accepted an empty address")
	}
}

// TestConsoleJoinEndToEnd runs the whole bridged join in one process against stand-ins: a fake nex surface (list, join with the
// session key, leave) and a PIA host (pia5.Host, which probes and answers like a console host) whose game side answers with the
// captured host's messages (FF hello, 02 pre-login reply, login response, D6). It checks the order of nex calls, the joiner's
// station URL, and that the joiner gets through traversal, mesh, login and into the world, then leaves.
func TestConsoleJoinEndToEnd(t *testing.T) {
	key := make([]byte, 32)
	copy(key, "fedcba9876543210fedcba9876543210")
	const gid, hostPID, pid = 9, 1570020176, 1358756791
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hostAddr := netip.MustParseAddrPort(conn.LocalAddr().String())
	c, _ := pia5.NewCipher(key, gid)
	h := pia5.NewHost(c, hostAddr, hostPID, func(to netip.AddrPort, hd pia5.Header, n [8]byte, msgs []pia5.Message) {
		conn.WriteTo(c.Seal(hd, n, msgs), net.UDPAddrFromAddrPort(to))
	})
	var mu sync.Mutex
	var gotLogin bool
	h.NewPeer = func(p *pia5.Peer) {
		p.OnGame = func(m []byte) {
			switch {
			case m[0] == 0xFE:
				p.Send(lce.Tmpl("hello_ff.bin"))
			case len(m) > 2 && m[2] == 0xE8:
				p.Send(append([]byte{0, 1, 0, 0, 0, 0, 0}, lce.Tmpl("prelogin.bin")...))
			case len(m) > 2 && m[2] == 0x01:
				mu.Lock()
				gotLogin = true
				mu.Unlock()
				d6, _ := hex.DecodeString("d6c061b000000000004052400000000000407028000000000001000000000000000001")
				p.Send(append(append([]byte{0, 1}, lce.Tmpl("login.bin")...), d6...))
			}
		}
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			h.Handle(from.(*net.UDPAddr).AddrPort(), append([]byte(nil), buf[:n]...))
		}
	}()
	go func() {
		for range time.Tick(100 * time.Millisecond) {
			h.Tick()
		}
	}()
	var calls []string
	var station string
	nex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unauthorized", 401)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		hs := fmt.Sprintf("prudp:/address=%s;port=%d;PID=%d;RVCID=7;natf=1;natm=1;type=3", hostAddr.Addr(), hostAddr.Port(), hostPID)
		switch r.URL.Path {
		case "/internal/minecraft/gatherings":
			json.NewEncoder(w).Encode([]Gathering{{Gathering: gid, OwnerPID: hostPID, HostPID: hostPID, Stations: []string{hs}}})
		case "/internal/minecraft/join":
			var q struct {
				Gathering, PID uint32
				Stations       []string
			}
			json.NewDecoder(r.Body).Decode(&q)
			if q.Gathering != gid || q.PID != pid || len(q.Stations) != 1 {
				http.Error(w, "bad join", 400)
				return
			}
			mu.Lock()
			station = q.Stations[0]
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"host_pid": hostPID, "stations": []string{hs}, "name": "WiiTest", "session_key": hex.EncodeToString(key)})
		case "/internal/minecraft/leave":
			w.Write([]byte("ok\n"))
		}
	}))
	defer nex.Close()
	res, err := ConsoleJoin{Nex: Nex{URL: nex.URL, Key: "k"}, PID: pid, Hold: 2 * time.Second}.Run(context.Background())
	if err != nil {
		t.Fatalf("%v (%+v)", err, res)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%+v", res)
	if !res.InWorld || !res.Login || !gotLogin || res.Traversed == 0 || res.Host != hostAddr.String() {
		t.Fatalf("incomplete: %+v", res)
	}
	if strings.Join(calls, ",") != "GET /internal/minecraft/gatherings,POST /internal/minecraft/join,POST /internal/minecraft/leave" {
		t.Fatalf("nex calls %v", calls)
	}
	if want := fmt.Sprintf(";PID=%d;R=0;RVCID=%d;", pid, JoinerRVCID); !strings.Contains(station, want) || !strings.HasPrefix(station, "prudp:/address=127.0.0.1;port=") {
		t.Fatalf("station %s", station)
	}
}
