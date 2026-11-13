package ps3join

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/lce"
	"github.com/openpak/minecraft/internal/ps3m2"
	"github.com/openpak/minecraft/internal/ps3p2p"
	"github.com/openpak/minecraft/internal/rtsig"
)

// Config is one probe run. Every field has a flag or environment variable in cmd/minecraft/joinps3.go.
type Config struct {
	Label, OutDir string

	// RT signalling (np-community): address, TLS, the NP domain of the probe account, the ticket file (np-matching2 `ticket`, never printed).
	RT         string
	RTTLS      bool
	Domain     string
	TicketFile string
	HostID     string // the hosting player's online id (selects the room, builds the host JID)
	HostJID    string // default <HostID>@<Domain>/ps3

	// Matching2: the agent's plain and TLS endpoints, the firmware hostname the TLS front routes on, the titles JSON (NP_MATCHING_TITLES:
	// {"<comm>": {"passphrase": "<256 hex>"}}) and the communication id.
	M2Plain, M2TLS, M2Name string
	TitlesFile, Comm       string
	OnlineID, Country      string // the probe's account; country defaults to the domain's first label
	Room                   string // 12 hex digits of the room id's low 48 bits; default: the host's room, else the first

	PublicIP, LocalIP string
	Port              int
	STUN              string
	WaitOffer         time.Duration // let the host offer first; then we offer
	Hold              time.Duration
	Pad               int
	// Service use (internal/bridge): Conn is a UDP socket the caller opened (Port is its port; nil: listen on Port), Rec a recorder the caller
	// owns (Run does not close it), NewGame replaces the joiner's own game layer (the bridge relays a console's stream instead of playing),
	// OnProgress hears every state change.
	Ticket, Pass []byte // the NP ticket and the title passphrase as bytes, instead of TicketFile and TitlesFile
	Conn         *net.UDPConn
	Rec          *Recorder
	NewGame      func(GameEnv) GameLayer
	OnProgress   func(what string, p Progress)

	PreLogin bool // run the login (E8 PreLogin, 01 Login, keep-alive, position: Game) after the hello, as a PS3 joiner does

	// Nudge59 is a WORKAROUND (0 = off; the joinps3 flag defaults to 10 s): this long after signalling is established, if the host has still
	// not written our table entry, set our 0x59 to 0 and then back to our mask with SetRoomMemberDataInternal (0x3206). np-matching2 pushes
	// each change to the host as 0x320d; the host's 0x1107 handler (EB 0xc96ea0) adds the new pads exactly as its 0x3207 path would: AddPlayer,
	// 0x3204, RUDP Initiate. Why the host does not do that on its 0x3207 reply (EB 0xc96560 event 0x10c, the path a real joiner takes) is not
	// known (mc4: nothing; mc5: only with this).
	Nudge59 time.Duration
}

// GameEnv is what a game layer gets when the hello leaves: who we are on the PS3 side, who the host is (entry 0 of its table), the writer.
type GameEnv struct {
	Name      string
	UID, Host [28]byte
	Small     byte
	// Write sends game bytes to the host (cut into 1,346-byte messages by the transport).
	Write func(b []byte)
	Logf  func(tag, format string, a ...any)
	// Leave ends the run: the host is gone or the session is over (reason is logged).
	Leave func(reason string)
}

// GameLayer is a game layer plugged into the joiner: Start after the hello, OnGame for every message of the host, Stop at the end.
type GameLayer interface {
	Start()
	OnGame(b []byte)
	Stop()
}

// nudgeStep is the pause between the experiment's two 0x59 writes (a var so the test can shorten it).
var nudgeStep = 3 * time.Second

// Progress is how far a run got.
type Progress struct {
	Signalling, Established, RudpConnected, TableEntry, HelloSent, FirstGame bool
	// Game is what the host's game stream showed (set when the run ends): pre-login reply, login response, in world, packets, chunks, position.
	Game lce.ClientStats
}

func (p Progress) String() string {
	y := func(b bool) string {
		if b {
			return "yes"
		}
		return "NO"
	}
	return fmt.Sprintf("signalling=%s established=%s rudp_connected=%s table_entry_seen=%s hello_sent=%s first_game_packet=%s",
		y(p.Signalling), y(p.Established), y(p.RudpConnected), y(p.TableEntry), y(p.HelloSent), y(p.FirstGame))
}

type probe struct {
	cfg Config
	rec *Recorder
	udp *net.UDPConn
	p   *rtsig.Participant

	mu        sync.Mutex
	prog      Progress
	hostAddr  *net.UDPAddr
	conn      *ps3p2p.Conn
	msgr      *ps3p2p.Msgr
	member    uint16
	roomKey   []byte
	small     byte
	uid       [28]byte
	gl        GameLayer // the game layer, made when the hello leaves
	hostUID   [28]byte  // entry 0 of the host's player table
	leave     chan string
	wantRudp  string
	rudpFrom  string
	helloDone bool
	m2        *ps3m2.Client
	estOnce   sync.Once
}

func (s *probe) progress(f func(*Progress), what string) {
	s.mu.Lock()
	before := s.prog
	f(&s.prog)
	after := s.prog
	s.mu.Unlock()
	if before != after {
		s.rec.Logf("PROGRESS", "%s: %s", what, after)
		if s.cfg.OnProgress != nil {
			s.cfg.OnProgress(what, after)
		}
	}
}

// Run joins and records until Hold elapses, ctx ends or the session dies; it always prints the one-line summary.
func Run(ctx context.Context, cfg Config) (Progress, error) {
	rec := cfg.Rec
	if rec == nil {
		var err error
		if rec, err = NewRecorder(cfg.OutDir, cfg.Label); err != nil {
			return Progress{}, err
		}
		defer rec.Close()
	}
	s := &probe{cfg: cfg, rec: rec, leave: make(chan string, 1)}
	err := s.run(ctx)
	s.mu.Lock()
	prog := s.prog
	s.mu.Unlock()
	prog.Game = s.gameStats()
	rec.Logf("SUMMARY", "%s", prog)
	rec.Logf("SUMMARY", "game: %s", StatsString(prog.Game, s.gameLost()))
	if err != nil {
		rec.Logf("SUMMARY", "stopped: %v", err)
	}
	return prog, err
}

func (s *probe) run(ctx context.Context) error {
	cfg, rec := s.cfg, s.rec
	if len(cfg.Domain) < 6 || cfg.HostID == "" || cfg.OnlineID == "" {
		return errors.New("need Domain (e.g. gb.gb.np.playstation.net), HostID and OnlineID")
	}
	if cfg.Country == "" {
		cfg.Country = cfg.Domain[:2]
		s.cfg = cfg
	}
	ticket, pass, err := cfg.Ticket, cfg.Pass, error(nil)
	if ticket == nil {
		if ticket, err = os.ReadFile(cfg.TicketFile); err != nil {
			return err
		}
	}
	if pass == nil {
		if pass, err = loadPass(cfg.TitlesFile, cfg.Comm); err != nil {
			return err
		}
	}
	// RT first: the host's offers toward a new member must find us signed in (run-live.sh learned this on Terraria).
	cl, err := rtsig.DialRT(cfg.RT, cfg.RTTLS, cfg.Domain, ticket)
	if err != nil {
		return fmt.Errorf("RT sign-in: %w", err)
	}
	defer cl.Close()
	rec.Logf("RT", "signed in as %s", cl.JID)
	if s.udp = cfg.Conn; s.udp == nil {
		if s.udp, err = net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.Port}); err != nil {
			return err
		}
	}
	defer s.udp.Close()
	var sid uint16
	binary.Read(rand.Reader, binary.BigEndian, &sid)
	if sid == 0 {
		sid = 1
	}
	skey := make([]byte, 16)
	rand.Read(skey)
	self := rtsig.Candidate{Type: "stun", Address: cfg.PublicIP, Port: uint16(cfg.Port)}
	if cfg.STUN != "" && cfg.STUN != "off" {
		if m, err := rtsig.STUNMapped(s.udp, cfg.STUN); err == nil {
			self = m
			rec.Logf("STUN", "mapped endpoint %s:%d", m.Address, m.Port)
		} else {
			rec.Logf("STUN", "%v (offering the configured candidate)", err)
		}
	}
	hostJID := cfg.HostJID
	if hostJID == "" {
		hostJID = cfg.HostID + "@" + cfg.Domain + "/ps3"
	}
	s.p = &rtsig.Participant{Self: self, SID: sid, SKey: skey, HostJID: hostJID, SelfJID: cl.JID}
	s.p.OfferCandidates = []rtsig.Candidate{self, {Type: "static", Address: "0.0.0.0", Port: uint16(cfg.Port)}}
	if cfg.LocalIP != "" {
		s.p.OfferCandidates = append(s.p.OfferCandidates, rtsig.Candidate{Type: "local", Address: cfg.LocalIP, Port: uint16(cfg.Port)})
	}
	s.p.Send = func(x string) error { rec.Logf("RT>>", "%s", x); return cl.Send(x) }
	cl.Log = func(x string) { rec.Logf("RT<<", "%s", x) }
	s.p.OnProbe = s.onProbe
	s.p.OnReject = func(st rtsig.Stanza) {
		rec.Logf("SIGNAL", "the host REJECTED our %s (iq %s): error code %s source %s (0e = no connection with these NpIds/sids)", st.Action, st.ID, st.ErrCode, st.ErrSource)
	}
	s.p.OnMutual = func(c rtsig.Candidate) {
		rec.Logf("SIGNAL", "MUTUAL ACTIVATED, host endpoint %s:%d", c.Address, c.Port)
		s.progress(func(p *Progress) { p.Signalling, p.Established = true, true }, "mutual activation")
		s.want("mutual activation")
		s.afterEstablished()
	}
	go s.readLoop()
	rtErr := make(chan error, 1)
	go func() { rtErr <- cl.Run(s.p) }()
	time.AfterFunc(cfg.WaitOffer, func() {
		if err := s.p.Start(); err != nil {
			rec.Logf("SIGNAL", "our offer: %v", err)
		}
	})
	go s.ticker(ctx)

	// Matching2: join the host's room as a console does.
	m2, err := s.matching2(ticket, pass)
	if err != nil {
		return fmt.Errorf("matching2: %w", err)
	}
	defer m2.Close()
	s.mu.Lock()
	s.m2 = m2
	s.mu.Unlock()
	defer func() { // leave the room so the next run joins fresh (the host raises MemberJoined only for a new member)
		s.stopGame() // the keep-alive loop first: nothing is sent to a host whose room member is gone
		m2.Leave()
		time.Sleep(500 * time.Millisecond)
	}()
	evErr := make(chan error, 1)
	go func() { evErr <- m2.Events(func(f *ps3m2.Frame) { s.onFrame(f) }) }()

	// Hold: stay in the world until Hold is over (keep-alive and position are the game layer's), say how it goes every 30 s, and leave early
	// only when the host drops us (FF Disconnect) or a connection to a service is gone.
	hold := time.After(cfg.Hold)
	report, gone := time.NewTicker(30*time.Second), time.NewTicker(time.Second)
	defer report.Stop()
	defer gone.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-hold:
			rec.Logf("DONE", "hold of %s over", cfg.Hold)
			return nil
		case why := <-s.leave:
			rec.Logf("DONE", "%s", why)
			return nil
		case err := <-rtErr:
			return fmt.Errorf("RT stream: %w", err)
		case err := <-evErr:
			return fmt.Errorf("matching2 connection: %w", err)
		case <-report.C:
			if st := s.gameStats(); st != (lce.ClientStats{}) {
				rec.Logf("GAME", "%s", StatsString(st, s.gameLost()))
			}
		case <-gone.C:
			if st := s.gameStats(); st.Disconnect != "" {
				rec.Logf("DONE", "the host dropped us: FF Disconnect reason %s (%s)", st.Disconnect, StatsString(st, s.gameLost()))
				return nil
			}
		}
	}
}

func (s *probe) gameStats() lce.ClientStats {
	s.mu.Lock()
	g := s.gl
	s.mu.Unlock()
	if st, ok := g.(interface{ Stats() lce.ClientStats }); ok {
		return st.Stats()
	}
	return lce.ClientStats{}
}

func (s *probe) gameLost() error {
	s.mu.Lock()
	g := s.gl
	s.mu.Unlock()
	if l, ok := g.(interface{ Lost() error }); ok {
		return l.Lost()
	}
	return nil
}

func (s *probe) stopGame() {
	s.mu.Lock()
	g := s.gl
	s.mu.Unlock()
	if g != nil {
		g.Stop()
	}
}

func loadPass(file, comm string) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	h := strings.TrimSpace(string(raw))
	if strings.HasPrefix(h, "{") {
		var cfg map[string]struct{ Passphrase string }
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		t, ok := cfg[comm]
		if !ok {
			return nil, fmt.Errorf("%s: no title %s", file, comm)
		}
		h = t.Passphrase
	}
	p, err := hex.DecodeString(h)
	if err != nil || len(p) != 128 {
		return nil, errors.New("title passphrase: want 256 hex digits")
	}
	return p, nil
}

// matching2 runs the join flow of spec §3.1 steps 2-5 against the room server.
func (s *probe) matching2(ticket, pass []byte) (*ps3m2.Client, error) {
	cfg, rec := s.cfg, s.rec
	c, err := ps3m2.Dial(cfg.M2Plain, cfg.M2TLS, cfg.M2Name)
	if err != nil {
		return nil, err
	}
	c.Comm, c.Pass = cfg.Comm, pass
	f, err := c.Ping()
	if err != nil {
		return nil, fmt.Errorf("GetServerInfo: %w", err)
	}
	if st := f.Elements()[0x1d]; len(st) >= 3 {
		rec.Logf("M2", "GetServerInfo ok, server %d status %d (the game needs 1)", binary.BigEndian.Uint16(st), st[2])
	}
	if _, err := c.Login(ticket, cfg.OnlineID, cfg.Country); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	rec.Logf("M2", "logged in as %s", cfg.OnlineID)
	if _, err := c.Call(ps3m2.CmdCreateServer, ps3m2.TLV(0x29, 4, []byte{0, 1})); err != nil {
		rec.Logf("M2", "CreateServerContext (0x1209): %v (continuing)", err)
	} else {
		rec.Logf("M2", "CreateServerContext ok")
	}
	sf, err := c.Call(ps3m2.CmdSearchRoom)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	rooms := ps3m2.ParseSearch(sf.Elements()[0x03])
	rec.Logf("M2", "search: %d rooms", len(rooms))
	var pick *ps3m2.SearchItem
	for i, r := range rooms {
		id := fmt.Sprintf("%04x%04x%04x", r.ID[0], r.ID[1], r.ID[2])
		rec.Logf("M2", "  room %s owner %s", id, r.Owner)
		if pick == nil && (cfg.Room == id || (cfg.Room == "" && (cfg.HostID == "" || cfg.HostID == r.Owner))) {
			pick = &rooms[i]
		}
	}
	if pick == nil {
		return nil, errors.New("no matching room (is the host online and hosting?)")
	}
	// Address the host by its own NpId, before JoinRoom: the console's offer toward us comes before the join reply.
	if err := s.p.SetHostNpID(pick.OwnerNpID); err != nil {
		rec.Logf("SIGNAL", "owner NpId %x: %v (keeping host JID %s)", pick.OwnerNpID, err, s.p.HostJID)
	} else {
		rec.Logf("SIGNAL", "host JID %s, from the room owner's NpId", s.p.HostJID)
	}
	low := binary.BigEndian.AppendUint16(nil, pick.ID[2])
	// step 2: the discovery data the game checks before it joins (both external attrs, version 0x0884, +0xad and +0xaf non-zero)
	// asked the way the game's firmware asks (2026-11-13 16:02Z, real PS3): the room named by bits 47..32 (0x29) and 15..0 (0x3d) of its id, the
	// attribute ids with their type nibble, and the four owner-info ids the library adds; plain 0x0055/0x0056 answered with fewer rooms than this
	rid := func(b uint16) []byte { return binary.BigEndian.AppendUint16(nil, b) }
	lf, err := c.Call(ps3m2.CmdGetExtList, ps3m2.TLV(0x29, 4, rid(pick.ID[0])), ps3m2.TLV(0x3d, 4, low), ps3m2.TLV(0x05, 4, rid(0x100c)),
		ps3m2.TLV(0x05, 4, rid(0x100d)), ps3m2.TLV(0x05, 4, rid(0x000b)), ps3m2.TLV(0x05, 4, rid(0x206c)), ps3m2.TLV(0x05, 4, rid(0x2055)), ps3m2.TLV(0x05, 4, rid(0x2056)))
	if err != nil {
		rec.Logf("M2", "GetRoomDataExternalList: %v (continuing)", err)
	} else {
		for i, a := range ps3m2.ParseExternalList(lf.Elements()[0x02]) {
			sess := append(append([]byte(nil), a[0x55]...), a[0x56]...)
			rec.Dump("M2", fmt.Sprintf("room %d external attrs 0x55+0x56 (%d+%d bytes)", i, len(a[0x55]), len(a[0x56])), sess)
			if len(sess) >= 0xb0 {
				rec.Logf("M2", "  session: version %#04x, +0xad=%d +0xae=%d +0xaf=%d (the game's list needs version 0x0884, +0xad and +0xaf non-zero)",
					binary.BigEndian.Uint16(sess), sess[0xad], sess[0xae], sess[0xaf])
			}
		}
	}
	// step 4: JoinRoom with the pad mask (member attr 0x59) and the public endpoint a console publishes
	join := [][]byte{ps3m2.TLV(0x3e, 4, low), ps3m2.MemberBin(s.mask())}
	if ip := net.ParseIP(cfg.PublicIP).To4(); ip != nil {
		join = append(join, ps3m2.TLV(0x6e, 2, ip), ps3m2.TLV(0x13, 3, []byte{2}))
	}
	jf, err := c.Call(ps3m2.CmdJoinRoom, join...)
	if err != nil {
		return nil, fmt.Errorf("JoinRoom: %w", err)
	}
	if jf.Result != 0 {
		return nil, fmt.Errorf("JoinRoom refused, result %#x", jf.Result)
	}
	el := jf.Elements()
	if v := el[0x16]; len(v) == 2 {
		s.member = binary.BigEndian.Uint16(v)
	}
	if tok := el[0x24]; len(tok) > 0 {
		if s.roomKey, err = ps3m2.RoomKeyFromToken(c.Key, tok); err != nil {
			rec.Logf("M2", "room key: %v", err)
		}
	}
	rec.Logf("M2", "JoinRoom ok: our member id %#04x", s.member)
	for _, m := range ps3m2.ParseMembers(el[0x02]) {
		rec.Logf("M2", "  member %#04x %s owner=%v 0x59=%x", m.ID, m.Name, m.Owner, m.Bin59)
	}
	a57, a58 := ps3m2.RoomAttrs(el[0x71], 0x20)
	s.uid = PlayerUID(cfg.OnlineID, cfg.Pad, [6]byte{0x00, 0x1f, 0xa7, 0x0f, 0x00, 0x01}, 1)
	s.updateTable(a57, a58, "join reply")
	return c, nil
}

// onFrame handles whatever the room server sends after the join: room events (MAC checked against the room key) and the replies to the
// requests sent after Events started (MAC checked against the session key).
func (s *probe) onFrame(f *ps3m2.Frame) {
	key, mac := s.roomKey, "no room key"
	if f.Kind == ps3m2.KindReply {
		s.mu.Lock()
		if s.m2 != nil {
			key = s.m2.Key
		}
		s.mu.Unlock()
	}
	if key != nil {
		mac = "MAC bad"
		if f.Verify(key) {
			mac = "MAC ok"
		}
	}
	s.rec.Logf("M2-EVENT", "kind %d cmd %#04x result %d len %d %s hex=%s", f.Kind, f.Cmd, f.Result, len(f.Raw), mac, Hex(f.Raw))
	if f.Kind == ps3m2.KindReply && f.Cmd == ps3m2.CmdGetMember {
		if m, ok := ps3m2.ParseMember(f.Elements()[0x4b]); ok {
			s.rec.Logf("M2", "diag: 0x3207 reply: member %#04x %s 0x59=%x (the host adds that many pads; empty 0x59 or no 0x4b = the host adds nobody)", m.ID, m.Name, m.Bin59)
		} else {
			s.rec.Logf("M2", "diag: 0x3207 reply has no 0x4b member: a host on this server never adds a joiner through its 0x5102 path")
		}
	}
	if f.Cmd == ps3m2.EvRoomData {
		a57, a58 := ps3m2.RoomAttrs(f.Body, 0)
		s.updateTable(a57, a58, "0x320e UpdatedRoomDataInternal")
	}
}

func (s *probe) updateTable(a57, a58 []byte, from string) {
	es := ParseTable(a57, a58)
	s.rec.Logf("TABLE", "%s: %d entries", from, len(es))
	if len(es) > 0 && es[0].Index == 0 && es[0].UID != [28]byte{} {
		s.mu.Lock()
		s.hostUID = es[0].UID
		s.mu.Unlock()
	}
	for _, e := range es {
		s.rec.Logf("TABLE", "  [%d] member %#04x small id %d pad %d name %q uid=%x", e.Index, e.Member, e.Small, e.Pad, e.Name(), e.UID)
	}
	if e, ok := Find(es, s.member); ok && s.member != 0 {
		s.mu.Lock()
		s.small = e.Small
		s.mu.Unlock()
		s.progress(func(p *Progress) { p.TableEntry = true }, "the host wrote our table entry")
		s.maybeHello()
	}
}

// endRun asks Run to return (the first reason counts).
func (s *probe) endRun(reason string) {
	select {
	case s.leave <- reason:
	default:
	}
}

func (s *probe) mask() []byte { return []byte{0, 0, 0, 1 << s.cfg.Pad} }

func (s *probe) afterEstablished() { s.estOnce.Do(func() { go s.postEstablished() }) }

// postEstablished runs once signalling is established. The host's game now (EB 0xc9735c, event 0x5102) asks the room server for our 0x59
// with GetRoomMemberDataInternal (0x3207) and does everything else, AddPlayer, 0x3204 with our table entry, its P2P socket, RUDP Initiate,
// only in the handler of that reply (EB 0xc96560 event 0x10c). We send the same request for ourselves so the log shows what the host gets.
// With Nudge59 set, the 0x1107 route is tried as well (Config.Nudge59).
func (s *probe) postEstablished() {
	s.mu.Lock()
	m2, member := s.m2, s.member
	s.mu.Unlock()
	if m2 == nil || member == 0 {
		s.rec.Logf("M2", "diag: no Matching2 session or member id at signalling Established, nothing sent")
		return
	}
	id := []byte{byte(member >> 8), byte(member)}
	s.rec.Logf("M2", "diag: GetRoomMemberDataInternal(our member %#04x, attr 0x59), the request the host sends on 0x5102 (EB 0xc97438)", member)
	if err := m2.Send(ps3m2.CmdGetMember, ps3m2.TLV(0x16, 4, id), ps3m2.TLV(0x05, 4, []byte{0, 0x59})); err != nil {
		s.rec.Logf("M2", "diag: 0x3207: %v", err)
	}
	if s.cfg.Nudge59 <= 0 {
		return
	}
	time.Sleep(s.cfg.Nudge59)
	s.mu.Lock()
	seen := s.prog.TableEntry
	s.mu.Unlock()
	if seen {
		s.rec.Logf("EXPERIMENT", "nudge59 skipped: the host already wrote our table entry (its 0x3207 path works)")
		return
	}
	for i, v := range [][]byte{{0, 0, 0, 0}, s.mask()} {
		s.rec.Logf("EXPERIMENT", "nudge59 step %d/2: SetRoomMemberDataInternal 0x59=%x; np-matching2 pushes 0x320d, the host's 0x1107 handler (EB 0xc96ea0) adds the new pads: AddPlayer, 0x3204, RUDP Initiate", i+1, v)
		if err := m2.Send(ps3m2.CmdSetMember, ps3m2.TLV(0x16, 4, id), ps3m2.MemberBin(v)); err != nil {
			s.rec.Logf("EXPERIMENT", "nudge59 step %d: %v", i+1, err)
		}
		time.Sleep(nudgeStep)
	}
	s.rec.Logf("EXPERIMENT", "nudge59 done: success = a TABLE line with our member id (0x320e) and UDP-RX kind-6 from the host; nothing = the host never got 0x320d")
}

func (s *probe) onProbe(c rtsig.Candidate) {
	s.rec.Logf("SIGNAL", "host candidate %s:%d, probing", c.Address, c.Port)
	s.progress(func(p *Progress) { p.Signalling = true }, "host offer")
	go func() {
		for i := 0; i < 40; i++ {
			if body, err := s.p.ProbePacketFor(); err == nil {
				pkt := rtsig.FrameSignal(body)
				for _, t := range s.p.ProbeTargets() {
					dst := &net.UDPAddr{IP: net.ParseIP(t.Address), Port: int(t.Port)}
					if dst.IP == nil || dst.Port == 0 || dst.IP.IsUnspecified() {
						continue
					}
					s.udp.WriteToUDP(pkt, dst)
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
}

func (s *probe) want(why string) {
	s.mu.Lock()
	if s.wantRudp == "" {
		s.wantRudp = why
	}
	s.mu.Unlock()
}

func (s *probe) ticker(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.startRudp()
			s.mu.Lock()
			c := s.conn
			s.mu.Unlock()
			if c != nil {
				c.Tick(now)
			}
			s.maybeHello()
		}
	}
}

// startRudp creates the RUDP context and sends the first SYN once signalling asked for it and both the host's skey and its address are known
// (the joiner's EB 0xc943e0: one context per own pad, port = pad + 1, Initiate toward the address the signalling chose).
func (s *probe) startRudp() {
	s.mu.Lock()
	if s.conn != nil || s.wantRudp == "" {
		s.mu.Unlock()
		return
	}
	peer := s.p.PeerSKey()
	dst := s.hostAddr
	if dst == nil {
		if c := s.p.HostCandidate(); c.Address != "" {
			dst = &net.UDPAddr{IP: net.ParseIP(c.Address), Port: int(c.Port)}
		}
	}
	if len(peer) != 16 || dst == nil || dst.IP == nil {
		s.mu.Unlock()
		return
	}
	var isn uint16
	var cid uint32
	binary.Read(rand.Reader, binary.BigEndian, &isn)
	binary.Read(rand.Reader, binary.BigEndian, &cid)
	c := ps3p2p.NewConn(uint16(s.cfg.Pad+1), isn, cid, func(frame []byte) { s.sendDatagram(peer, frame) })
	c.Trace = func(dir, desc string, pkt []byte) { s.rec.Logf("RUDP", "%s %s hex=%s", dir, desc, Hex(pkt)) }
	c.OnEstablished = func() {
		s.rec.Logf("RUDP", "ESTABLISHED (peer MSS %d)", c.PeerMSS())
		s.progress(func(p *Progress) { p.RudpConnected = true }, "rudp context established")
		s.maybeHello()
	}
	c.OnReset = func() {
		s.rec.Logf("RUDP", "the peer sent a RESET")
		if s.cfg.NewGame != nil {
			s.endRun("the host's RUDP connection was reset")
		}
	}
	m := &ps3p2p.Msgr{C: c}
	m.OnGame = s.onGame
	m.OnAck = func() { s.rec.Logf("MSG", "rx ack (kind 3), %d kind-2 in flight", m.Inflight()) }
	m.Trace = func(dir string, kind uint16, p []byte, note string) {
		s.rec.Logf("MSG", "%s kind %d len %d %s", dir, kind, len(p), note)
		if len(p) > 0 {
			s.rec.Dump("MSG", fmt.Sprintf("%s kind %d payload", dir, kind), p)
		}
		if dir == "tx" && kind != ps3p2p.KindAck {
			s.rec.Game("tx", p)
		}
	}
	m.Attach()
	s.conn, s.msgr, s.rudpFrom = c, m, s.wantRudp
	s.mu.Unlock()
	s.rec.Logf("RUDP", "starting (%s): port %d toward %s", s.rudpFrom, s.cfg.Pad+1, dst)
	s.rec.Keys(s.cfg.OutDir, s.cfg.Label, s.p.SKey, peer)
	s.mu.Lock()
	s.hostAddr = dst
	s.mu.Unlock()
	c.Start(time.Now())
}

func (s *probe) sendDatagram(peerKey, frame []byte) {
	d, err := ps3p2p.SealRandom(peerKey, frame)
	if err != nil {
		s.rec.Logf("UDP", "seal: %v", err)
		return
	}
	s.mu.Lock()
	dst := s.hostAddr
	s.mu.Unlock()
	if _, err := s.udp.WriteToUDP(d, dst); err != nil {
		s.rec.Logf("UDP", "send to %s: %v", dst, err)
		return
	}
	s.rec.Logf("UDP-TX", "to %s %d bytes raw=%s plain=%s", dst, len(d), Hex(d), Hex(frame))
}

// maybeHello sends the 32-byte hello once, and only when RUDP is established AND the host has written our entry in its table (spec §3.1
// step 9: the joiner speaks first at the game layer but not before; a host waiting for the hello before it writes the table deadlocks).
func (s *probe) maybeHello() {
	s.mu.Lock()
	if s.helloDone || !s.prog.TableEntry || s.conn == nil || !s.conn.Established() {
		s.mu.Unlock()
		return
	}
	s.helloDone = true
	small, uid, m := s.small, s.uid, s.msgr
	write := func(b []byte) { m.WriteGame(b, ps3p2p.GuessKind2Counted, time.Now()) }
	// the game layer exists before the hello leaves: the host's first message can come right behind it
	if s.cfg.NewGame != nil {
		s.gl = s.cfg.NewGame(GameEnv{Name: s.cfg.OnlineID, UID: uid, Host: s.hostUID, Small: small, Write: write, Logf: s.rec.Logf, Leave: s.endRun})
	} else {
		s.gl = &Game{Name: s.cfg.OnlineID, UID: uid, Logf: s.rec.Logf, Write: write}
	}
	g := s.gl
	s.mu.Unlock()
	h := Hello(small, uid)
	s.rec.Logf("HELLO", "sending the 32-byte hello (small id %d): %x", small, h)
	if err := m.Write(ps3p2p.KindPlain, h, time.Now()); err != nil {
		s.rec.Logf("HELLO", "%v", err)
		return
	}
	s.progress(func(p *Progress) { p.HelloSent = true }, "hello sent")
	if s.cfg.PreLogin || s.cfg.NewGame != nil {
		// E8 PreLogin goes out at once (lce.Client.Start); the 01 Login follows the host's 02, then keep-alive and position once it is in the world
		s.rec.Logf("HELLO", "starting the login: E8 PreLogin as %q, then 01 Login after the host's 02", s.cfg.OnlineID)
		g.Start()
	}
}

// onGame records a kind 1 or 2 message of the host (the raw bytes to <label>.rx.bin) and hands it to the game layer, which walks, names and
// answers it. Nothing here stops on an unknown packet.
func (s *probe) onGame(kind uint16, p []byte) {
	s.progress(func(pr *Progress) { pr.FirstGame = pr.FirstGame || pr.HelloSent }, "game packet after our hello")
	s.rec.Game("rx", p)
	s.mu.Lock()
	g := s.gl
	s.mu.Unlock()
	if g != nil {
		g.OnGame(p)
	}
}

// readLoop reads the shared UDP socket: signalling probes first, kind-6 game datagrams after. It never stops on a bad datagram.
func (s *probe) readLoop() {
	buf := make([]byte, 4096)
	probed, established := false, false
	for {
		s.udp.SetReadDeadline(time.Now().Add(time.Hour))
		n, src, err := s.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		kind, sv, dv, body, uerr := rtsig.Unframe(pkt)
		switch {
		case uerr != nil:
			s.rec.Logf("UDP-RX", "UNFRAMED %d bytes from %s: %x", n, src, pkt)
		case kind == ps3p2p.Header[1]&0x0f && dv == 1:
			s.mu.Lock()
			s.hostAddr = src
			s.mu.Unlock()
			s.game(src, pkt)
		case dv == rtsig.VPortSignal && len(body) > 0 && body[0] == 4:
			s.rec.Logf("SIGNAL", "host echo from %s, answered", src)
			if r := rtsig.EchoReply(body); r != nil {
				s.udp.WriteToUDP(rtsig.FrameSignal(r), src)
			}
		case dv == rtsig.VPortSignal && s.isBandwidth(body):
			if !established {
				established = true
				s.rec.Logf("SIGNAL", "HOST ESTABLISHED (type-3 bandwidth pair from %s)", src)
				s.mu.Lock()
				s.hostAddr = src
				s.mu.Unlock()
				s.progress(func(p *Progress) { p.Signalling, p.Established = true, true }, "host established")
				s.want("host established")
				s.afterEstablished()
			}
		case dv == rtsig.VPortSignal && len(body) == 0x58 && body[0] <= 2:
			if !s.p.FromHost(body) {
				continue // our own probe, hairpinned back by the NAT
			}
			s.rec.Logf("SIGNAL", "host probe type %d from %s", body[0], src)
			if resp := s.p.ProbeResponseFor(body, src); resp != nil {
				s.udp.WriteToUDP(rtsig.FrameSignal(resp), src)
			}
			if !probed {
				probed = true
				s.mu.Lock()
				s.hostAddr = src
				s.mu.Unlock()
				go func() {
					time.Sleep(200 * time.Millisecond)
					if err := s.p.ProbeOK(); err != nil {
						s.rec.Logf("SIGNAL", "accept after probe: %v", err)
					}
				}()
			}
		default:
			s.rec.Logf("UDP-RX", "%d bytes from %s kind %d vports %#x->%#x: %x", n, src, kind, sv, dv, pkt)
		}
	}
}

func (s *probe) isBandwidth(body []byte) bool {
	_, ok := rtsig.Bandwidth(body)
	return ok && s.p.FromHost(body)
}

// game handles one kind-6 datagram: opened with our own skey; if only the PEER's skey verifies it, that is recorded (the key direction of
// the spec would be the wrong way round) and the datagram is used anyway so the session stays alive.
func (s *probe) game(src *net.UDPAddr, pkt []byte) {
	plain, seed, err := ps3p2p.Open(s.p.SKey, pkt)
	status := "MAC ok (own skey)"
	if err != nil {
		peer := s.p.PeerSKey()
		if p2, _, e2 := ps3p2p.Open(peer, pkt); len(peer) == 16 && e2 == nil {
			plain, status = p2, "MAC ok only with the PEER's skey (key direction differs from the spec)"
		} else {
			status = fmt.Sprintf("%v: plaintext below is what our own skey makes of it", err)
		}
	}
	s.rec.Logf("UDP-RX", "kind-6 from %s %d bytes seed %#08x %s raw=%s plain=%s", src, len(pkt), seed, status, Hex(pkt), Hex(plain))
	s.want("first kind-6 datagram")
	s.mu.Lock()
	c := s.conn
	s.mu.Unlock()
	if c == nil {
		s.startRudp()
		s.mu.Lock()
		c = s.conn
		s.mu.Unlock()
	}
	if c != nil {
		c.Handle(plain, time.Now())
	}
}
