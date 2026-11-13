package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/directory"
	"github.com/openpak/minecraft/internal/ps3join"
)

// Config is everything the PS3 leg needs from the box. Env names are in cmd/minecraft/serve.go.
type Config struct {
	// np-matching2's internal listener (GET /internal/bridge/identity, bearer Key = its NP_BRIDGE_KEY).
	Matching, Key string
	Comm          string // Minecraft PS3 communication id (NPWR05636_00)

	// What a PS3 console talks to: Matching2 plain and TLS, np-community's RT (the TLS 5223 front or the plain loopback port).
	M2Plain, M2TLS, M2Name string
	RT                     string
	RTTLS                  bool

	// The UDP socket the home PS3 reaches: bound to BindIP on one port of [PortLo, PortHi], announced as PublicIP:port in the signalling
	// candidates. The ports must be open to the PS3 (cloud security list and firewall).
	BindIP, PublicIP string
	PortLo, PortHi   int

	WaitOffer time.Duration // let the host offer first, then we offer
	Hold      time.Duration // longest session
	TraceDir  string        // per-session trace files (trace.go)

	// ReadyTimeout is how long the PS3 leg may take to get its hello out; WiiUTimeout how long the console may take to show up and how long it
	// may be silent.
	ReadyTimeout, WiiUTimeout time.Duration

	// ResolveWiiU maps a Wii U PID to its OpenPak account; Game finds a game of the session directory by id.
	ResolveWiiU func(ctx context.Context, pid uint32) (string, error)
	Game        func(id string) (directory.Game, bool)

	HTTP *http.Client
	// Run is the PS3 joiner (ps3join.Run; tests replace it).
	Run func(ctx context.Context, cfg ps3join.Config) (ps3join.Progress, error)
}

// Manager runs the sessions of Wii U consoles in PS3-hosted games. It implements relay.Bridge.
type Manager struct {
	Config
	mu       sync.Mutex
	sessions map[string]*Session
}

func (m *Manager) log() *slog.Logger { return slog.Default() }

func (m *Manager) def(d *time.Duration, v time.Duration) time.Duration {
	if *d <= 0 {
		return v
	}
	return *d
}

// Prepare starts the PS3 leg of a Wii U joiner (nex's push at JoinMatchmakeSession). It is idempotent while the session lives.
func (m *Manager) Prepare(game string, hostPID, joinerPID uint32) error {
	_, err := m.session(game, hostPID, joinerPID)
	return err
}

// Attach hands the console's PIA peer to the session nex's push started. A console with no session, or one whose session ended, is refused
// (its PIA peer is closed): a console that keeps sending after its session ended must not start a new PS3 join; only a new JoinMatchmakeSession
// (Prepare) does.
func (m *Manager) Attach(game string, hostPID, joinerPID uint32, send func([]byte), drop func()) (func([]byte), error) {
	m.mu.Lock()
	s := m.sessions[key(game, joinerPID)]
	m.mu.Unlock()
	switch {
	case s == nil:
		return nil, fmt.Errorf("no session for PID %d in %s: nex did not push the join", joinerPID, game)
	case s.Ended():
		return nil, fmt.Errorf("session ended")
	}
	return s.Attach(send, drop), nil
}

func key(game string, joinerPID uint32) string { return fmt.Sprintf("%s/%d", game, joinerPID) }

func (m *Manager) session(game string, hostPID, joinerPID uint32) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[key(game, joinerPID)]; s != nil && !s.Ended() {
		return s, nil
	}
	g, ok := m.Game(game)
	if !ok {
		return nil, fmt.Errorf("game %s is not in the session directory (gone, or not hosted on a PS3)", game)
	}
	if g.Platform != "ps3" {
		return nil, fmt.Errorf("game %s is hosted on %s: only PS3-hosted games are bridged to a Wii U joiner", game, g.Platform)
	}
	label := Label(game, joinerPID, time.Now())
	tr, err := NewTrace(m.TraceDir, label)
	if err != nil {
		return nil, fmt.Errorf("trace files in %s: %w", m.TraceDir, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := NewSession(game, hostPID, joinerPID, g.Settings.HostName, tr, m.log(), cancel)
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
	}
	m.sessions[key(game, joinerPID)] = s
	s.SetState("preparing", "trace "+label)
	go m.run(ctx, s, g)
	go m.watch(ctx, s)
	return s, nil
}

// watch ends a session that never gets going: the PS3 leg not in the game in time, the console never connecting, or the console gone silent.
func (m *Manager) watch(ctx context.Context, s *Session) {
	start := time.Now()
	readyIn, wiiuIn := m.def(&m.ReadyTimeout, 2*time.Minute), m.def(&m.WiiUTimeout, 90*time.Second)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ready, attached, idle := s.phase()
		switch {
		case !ready && time.Since(start) > readyIn:
			s.End(fmt.Sprintf("the PS3 leg was not in the room's game after %s (state %s)", readyIn, s.State()))
		case !attached && time.Since(start) > wiiuIn:
			s.End(fmt.Sprintf("the Wii U's PIA session never came up in %s", wiiuIn))
		case attached && idle > 20*time.Second:
			s.End("the Wii U went silent for 20 s")
		}
	}
}

// phase is whether the PS3 leg is in the game, whether the console's PIA peer is attached, and how long the console has been silent.
func (s *Session) phase() (ready, attached bool, idle time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready, s.send != nil, time.Since(s.lastRx)
}

type identity struct {
	OnlineID   string `json:"online_id"`
	Country    string `json:"country"`
	Domain     string `json:"domain"`
	Ticket     string `json:"ticket"`
	Comm       string `json:"comm"`
	Passphrase string `json:"passphrase"`
}

// identity asks np-matching2 who an account is on the PS3 network (the joiner: with a ticket and the title passphrase).
func (m *Manager) identity(ctx context.Context, account string, withTicket bool) (identity, error) {
	var id identity
	q := "?account=" + url.QueryEscape(account)
	if withTicket {
		q += "&ticket=1&comm=" + url.QueryEscape(m.Comm)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", m.Matching+"/internal/bridge/identity"+q, nil)
	if err != nil {
		return id, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Key)
	c := m.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return id, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return id, fmt.Errorf("np-matching2 identity: %d %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return id, json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&id)
}

// listen opens the PS3 leg's UDP socket on the first free port of the range.
func (m *Manager) listen() (*net.UDPConn, int, error) {
	ip := net.ParseIP(m.BindIP)
	var last error
	for p := m.PortLo; p <= m.PortHi; p++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: p})
		if err == nil {
			return c, p, nil
		}
		last = err
	}
	return nil, 0, fmt.Errorf("no free UDP port in %d-%d on %s: %w", m.PortLo, m.PortHi, m.BindIP, last)
}

// run is the PS3 leg: the joining player's identity, the room of the host, the real join; it returns when the leg is over.
func (m *Manager) run(ctx context.Context, s *Session, g directory.Game) {
	defer s.Trace.Close()
	fail := func(format string, a ...any) { s.End(fmt.Sprintf(format, a...)) }
	acct, err := m.ResolveWiiU(ctx, s.JoinerPID)
	if err != nil {
		fail("no OpenPak account behind Wii U PID %d: %v", s.JoinerPID, err)
		return
	}
	me, err := m.identity(ctx, acct, true)
	if err != nil {
		fail("PS3 identity of the joining player: %v", err)
		return
	}
	host, err := m.identity(ctx, g.HostAcct, false)
	if err != nil {
		fail("PS3 identity of the host: %v", err)
		return
	}
	ticket, err := base64.StdEncoding.DecodeString(me.Ticket)
	pass, err2 := hex.DecodeString(me.Passphrase)
	if err != nil || err2 != nil || len(ticket) == 0 || len(pass) != 128 {
		fail("np-matching2 sent no usable ticket or passphrase")
		return
	}
	conn, port, err := m.listen()
	if err != nil {
		fail("%v", err)
		return
	}
	s.SetState("ps3-joining", fmt.Sprintf("as %s (%s) into the room of %s, UDP %s:%d", me.OnlineID, me.Domain, host.OnlineID, m.PublicIP, port))
	run := m.Run
	if run == nil {
		run = ps3join.Run
	}
	prog, err := run(ctx, ps3join.Config{
		Rec: s.Trace.Rec, Ticket: ticket, Pass: pass, Conn: conn, Port: port,
		RT: m.RT, RTTLS: m.RTTLS, Domain: me.Domain, OnlineID: me.OnlineID, Country: me.Country, HostID: host.OnlineID, Comm: me.Comm,
		M2Plain: m.M2Plain, M2TLS: m.M2TLS, M2Name: m.M2Name, PublicIP: m.PublicIP,
		WaitOffer: m.def(&m.WaitOffer, 10*time.Second), Hold: m.def(&m.Hold, 6*time.Hour), Pad: 0, Nudge59: 10 * time.Second,
		NewGame: s.Layer, OnProgress: func(what string, p ps3join.Progress) { s.progress(what, p) },
	})
	if err != nil {
		fail("the PS3 leg stopped: %v (%s)", err, prog)
		return
	}
	fail("the PS3 leg is over (%s)", prog)
}

// progress turns the joiner's progress into the session's states.
func (s *Session) progress(what string, p ps3join.Progress) {
	switch {
	case p.HelloSent:
		// ps3-hello is set by Ready, once the game layer starts
	case p.TableEntry:
		s.SetState("ps3-room", what)
	case p.RudpConnected:
		s.SetState("ps3-rudp", what)
	case p.Established:
		s.SetState("ps3-established", what)
	case p.Signalling:
		s.SetState("ps3-signalling", what)
	}
}
