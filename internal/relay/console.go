package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/pia5"
)

// The bridge's Wii U-host leg (prds#177): join a world a real console hosts, as a Wii U joiner, on behalf of a player on another
// console. The NEX side is server-side (nex minecraft-wiiu /internal/minecraft/*); the PIA and game sides are pia5.Joiner and
// lce.Client.

// JoinerRVCID is the RendezVous connection id the bridge's joiner station announces. A console joiner's is its NEX connection
// id; the bridged player has no NEX connection, so it takes one no live connection has (the service host uses pia5.HostRVCID).
const JoinerRVCID = 0xFFF1

// Gathering is one console-hosted gathering, as nex lists it.
type Gathering struct {
	Gathering    uint32   `json:"gathering"`
	OwnerPID     uint64   `json:"owner_pid"`
	HostPID      uint64   `json:"host_pid"`
	Participants []uint64 `json:"participants"`
	Stations     []string `json:"stations"`
}

// Nex is the nex minecraft-wiiu internal surface.
type Nex struct {
	URL, Key string
}

func (n Nex) call(ctx context.Context, method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		json.NewEncoder(&body).Encode(in)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.URL, "/")+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+n.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var b [200]byte
		k, _ := resp.Body.Read(b[:])
		return fmt.Errorf("nex %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(b[:k])))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Gatherings lists the console-hosted gatherings whose host is online.
func (n Nex) Gatherings(ctx context.Context) ([]Gathering, error) {
	var g []Gathering
	return g, n.call(ctx, "GET", "/internal/minecraft/gatherings", nil, &g)
}

// stationAddr reads address and port from a NEX station URL.
func stationAddr(url string) (netip.AddrPort, bool) {
	_, params, ok := strings.Cut(url, ":/")
	if !ok {
		return netip.AddrPort{}, false
	}
	var addr, port string
	for _, kv := range strings.Split(params, ";") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "address":
			addr = v
		case "port":
			port = v
		}
	}
	a, err := netip.ParseAddr(addr)
	p, err2 := strconv.ParseUint(port, 10, 16)
	if err != nil || err2 != nil || p == 0 || a.IsUnspecified() {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(a, uint16(p)), true
}

// joinerStation is the station URL the joiner hands the host (through nex), shaped like a real console joiner's (nex log
// 2026-11-10 21:13:48: address, port, CID = the PIA station's CID, PID, RVCID, natf/natm, probeinit=1, sid 15, stream 0, type 3).
func joinerStation(self netip.AddrPort, cid, pid uint32) string {
	return fmt.Sprintf("prudp:/address=%s;port=%d;CID=%d;PID=%d;R=0;RVCID=%d;Ra=0.0.0.0;Rp=0;Rsa=0.0.0.0;Rsp=0;natf=1;natm=1;probeinit=1;sid=15;stream=0;type=3",
		self.Addr(), self.Port(), cid, pid, JoinerRVCID)
}

// ConsoleJoin is one bridged join.
type ConsoleJoin struct {
	Nex       Nex
	Gathering uint32 // 0: the first one nex lists
	PID       uint32 // the joining player's Wii U PID
	Name      string // "" = the account's name from nex
	Bind      string // local UDP address, ":0" by default
	Public    netip.Addr
	Hold      time.Duration
	Log       *slog.Logger
}

// ConsoleResult is what happened.
type ConsoleResult struct {
	Gathering         uint32
	HostPID           uint64
	Self, Host        string
	Traversed, Meshed time.Duration // since the join call
	Held              time.Duration
	lce.ClientStats
}

// Run joins and holds the session for Hold, or until the host goes silent for 30 s.
func (cj ConsoleJoin) Run(ctx context.Context) (ConsoleResult, error) {
	var res ConsoleResult
	log := cj.Log
	if log == nil {
		log = slog.Default()
	}
	gs, err := cj.Nex.Gatherings(ctx)
	if err != nil {
		return res, err
	}
	var g *Gathering
	for i := range gs {
		if cj.Gathering == 0 || gs[i].Gathering == cj.Gathering {
			g = &gs[i]
			break
		}
	}
	if g == nil {
		return res, fmt.Errorf("no console-hosted gathering with an online host (nex lists %d)", len(gs))
	}
	res.Gathering, res.HostPID = g.Gathering, g.HostPID
	var targets []netip.AddrPort
	for _, s := range g.Stations {
		if a, ok := stationAddr(s); ok && !containsAddr(targets, a) {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return res, fmt.Errorf("gathering %d: no host station address in %q", g.Gathering, g.Stations)
	}
	bind := cj.Bind
	if bind == "" {
		bind = ":0"
	}
	conn, err := net.ListenPacket("udp4", bind)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	claim := cj.Public
	if !claim.IsValid() {
		claim = routeTo(targets)
	}
	self := netip.AddrPortFrom(claim, port)
	res.Self = self.String()
	var r [4]byte
	rand.Read(r[:])
	cid := binary.BigEndian.Uint32(r[:])
	var jr struct {
		HostPID    uint32   `json:"host_pid"`
		Stations   []string `json:"stations"`
		Name       string   `json:"name"`
		SessionKey string   `json:"session_key"`
	}
	started := time.Now()
	if err := cj.Nex.call(ctx, "POST", "/internal/minecraft/join", map[string]any{"gathering": g.Gathering, "pid": cj.PID,
		"stations": []string{joinerStation(self, cid, cj.PID)}}, &jr); err != nil {
		return res, err
	}
	defer func() {
		lctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := cj.Nex.call(lctx, "POST", "/internal/minecraft/leave", map[string]any{"gathering": g.Gathering, "pid": cj.PID}, nil); err != nil {
			log.Warn("leave", "err", err)
		}
	}()
	key, err := hex.DecodeString(jr.SessionKey)
	jr.SessionKey = ""
	if err != nil {
		return res, fmt.Errorf("session key from nex: not hex")
	}
	cipher, err := pia5.NewCipher(key, g.Gathering)
	if err != nil {
		return res, err
	}
	name := cj.Name
	if name == "" {
		name = jr.Name
	}
	if name == "" {
		name = "Player" + strconv.Itoa(int(cj.PID%10000))
	}
	log.Info("joined gathering on nex", "gathering", g.Gathering, "host_pid", jr.HostPID, "as", cj.PID, "name", name, "self", self.String(), "targets", fmt.Sprint(targets))

	var mu sync.Mutex
	lastRx := time.Now()
	j := &pia5.Joiner{Conn: conn, Cipher: cipher, PID: cj.PID, HostPID: jr.HostPID, Self: self, CID: cid, RVCID: JoinerRVCID, Targets: targets,
		Joined: make(chan struct{})}
	if lp := netip.MustParseAddrPort(conn.LocalAddr().String()); lp.Addr().IsValid() && !lp.Addr().IsUnspecified() && lp.Addr() != claim {
		j.Private = lp
	}
	j.Debug = func(uint8, uint8, int) { mu.Lock(); lastRx = time.Now(); mu.Unlock() }
	j.OnTraversed = func(from netip.AddrPort) {
		mu.Lock()
		defer mu.Unlock()
		res.Traversed = time.Since(started)
		res.Host = from.String()
		log.Info("NAT traversal done", "host", from.String(), "after", res.Traversed.Round(time.Millisecond).String())
	}
	cl := &lce.Client{Send: j.SendGame, Name: name, PID: cj.PID, HostPID: jr.HostPID, Log: log}
	j.OnGame = cl.OnGame
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go j.Run(jctx)
	select {
	case <-j.Joined:
	case <-time.After(20 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		return res, fmt.Errorf("no mesh after 20 s (NAT traversal done: %v)", res.Traversed != 0)
	case <-ctx.Done():
		return res, ctx.Err()
	}
	mu.Lock()
	res.Meshed = time.Since(started)
	mu.Unlock()
	log.Info("mesh up", "after", res.Meshed.Round(time.Millisecond).String())
	cl.Start()
	defer cl.Stop()
	held := time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	report := time.Now()
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			defer mu.Unlock()
			res.Held, res.ClientStats = time.Since(held), cl.Stats()
			return res, ctx.Err()
		case <-t.C:
		}
		st := cl.Stats()
		mu.Lock()
		res.Held, res.ClientStats = time.Since(held), st
		idle := time.Since(lastRx)
		mu.Unlock()
		if st.Disconnect != "" {
			return res, fmt.Errorf("the host disconnected us (FF %s)", st.Disconnect)
		}
		if idle > 30*time.Second {
			return res, fmt.Errorf("the host has been silent for %s", idle.Round(time.Second))
		}
		if time.Since(report) >= 30*time.Second {
			report = time.Now()
			log.Info("holding", "held", res.Held.Round(time.Second).String(), "in_world", st.InWorld, "packets", st.Packets, "chunks", st.Chunks)
		}
		if res.Held >= cj.Hold {
			return res, nil
		}
	}
}

func containsAddr(l []netip.AddrPort, a netip.AddrPort) bool {
	for _, x := range l {
		if x == a {
			return true
		}
	}
	return false
}

// routeTo is the local address the kernel would send from toward the host: a private host address first (the same LAN), then
// any. ponytail: one address claimed as public and private; pass -public where NAT maps it (the box: 145.241.206.67).
func routeTo(targets []netip.AddrPort) netip.Addr {
	order := append([]netip.AddrPort(nil), targets...)
	for i, t := range order {
		if t.Addr().IsPrivate() {
			order[0], order[i] = order[i], order[0]
			break
		}
	}
	for _, t := range order {
		if c, err := net.Dial("udp4", t.String()); err == nil {
			a := netip.MustParseAddrPort(c.LocalAddr().String()).Addr()
			c.Close()
			return a
		}
	}
	return netip.IPv4Unspecified()
}
