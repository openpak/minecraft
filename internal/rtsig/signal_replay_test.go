package rtsig

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// Replays of two live Minecraft 01.84 runs against the real PS3, stanza for stanza (research/ps3/matching2-protocol.md §42).
// Logged skeys are replaced: the PS3's by hostKey, ours by ourKey ("SKEY" in the templates below picks by direction).

const (
	hostKey = "0123456789abcdef0123456789abcdef"
	ourKey  = "fedcba9876543210fedcba9876543210"
)

func fromHost(s string) string { return strings.Replace(s, "SKEY", hostKey, 1) }
func fromUs(s string) string   { return strings.Replace(s, "SKEY", ourKey, 1) }

func recorder(p *Participant) *[]string {
	var out []string
	p.Send = func(s string) error { out = append(out, s); return nil }
	return &out
}

func feed(t *testing.T, p *Participant, s string) {
	t.Helper()
	st, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Handle(st); err != nil {
		t.Fatal(err)
	}
}

func wantSent(t *testing.T, got *[]string, want ...string) {
	t.Helper()
	if len(*got) != len(want) {
		t.Fatalf("sent %d stanzas, want %d:\n%s", len(*got), len(want), strings.Join(*got, "\n"))
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("stanza %d\n got %s\nwant %s", i, (*got)[i], want[i])
		}
	}
	*got = nil
}

func key(s string) []byte { k, _ := hex.DecodeString(s); return k }

// mc3, 2026-11-11 16:31:00Z: the PS3 (hosting) offers first with psid 0.
var (
	mc3HostOffer = `<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" type="set" id="ps3-b3d79761" from="PS3Test@pt.pt.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="offer" sid="48959" psid="0" skey="SKEY" nattype="2"><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="stun" address="37.228.205.125" port="5358"></candidate><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="static" address="0.0.0.0" port="3658"></candidate><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="local" address="10.87.0.11" port="3658"></candidate></query></iq>`
	mc3OurAck    = `<iq type="result" id="ps3-b3d79761" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p"/></iq>`
	mc3OurOffer  = `<iq type="set" id="sig1" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="offer" sid="14471" psid="48959" skey="SKEY" nattype="1"><candidate type="stun" address="10.87.0.2" port="3658"/><candidate type="static" address="0.0.0.0" port="3658"/><candidate type="local" address="10.87.0.2" port="3658"/></query></iq>`
	mc3Error     = `<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" from="PS3Test@pt.pt.np.playstation.net/ps3" type="error" id="sig1"><query xmlns="http://playstation.net/np/rt/udpp2p" action="offer" sid="14471" psid="48959" skey="SKEY" nattype="1"><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="stun" address="10.87.0.2" port="3658"></candidate><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="static" address="0.0.0.0" port="3658"></candidate><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="local" address="10.87.0.2" port="3658"></candidate></query><error xmlns="jabber:client"><x xmlns="http://playstation.net/np/rt/error" code="0e" source="client"></x></error></iq>`
)

func mc3Participant() *Participant {
	return &Participant{SID: 14471, SKey: key(ourKey), HostJID: "PS3Test@pt.pt.np.playstation.net/ps3", SelfJID: "SwitchTest@gb.gb.np.playstation.net/ps3",
		Self:            Candidate{Type: "stun", Address: "10.87.0.2", Port: 3658},
		OfferCandidates: []Candidate{{Type: "stun", Address: "10.87.0.2", Port: 3658}, {Type: "static", Address: "0.0.0.0", Port: 3658}, {Type: "local", Address: "10.87.0.2", Port: 3658}}}
}

// consoleNpID is PS3Test's own NpId as its Matching2 login 0x0b carries it (§41): online id + "ptnpps3".
func consoleNpID() []byte {
	b := make([]byte, 24)
	copy(b, "PS3Test")
	copy(b[16:], "ptnpps3")
	return b
}

// The run as it went: our reply is byte-identical to the log, so the test replays the real exchange.
func TestMC3AsRecorded(t *testing.T) {
	p := mc3Participant()
	sent := recorder(p)
	feed(t, p, fromHost(mc3HostOffer))
	wantSent(t, sent, mc3OurAck, fromUs(mc3OurOffer))
}

// The PS3's error echoes our offer back. It must be reported, never acked, and never taken as an offer (the probe used to adopt
// its own candidates from it and probe 10.87.0.2:3658, i.e. itself).
func TestMC3ErrorIsARejection(t *testing.T) {
	p := mc3Participant()
	sent := recorder(p)
	var rej []Stanza
	p.OnReject = func(st Stanza) { rej = append(rej, st) }
	feed(t, p, fromHost(mc3HostOffer))
	*sent = nil
	feed(t, p, fromUs(mc3Error))
	wantSent(t, sent)
	if len(rej) != 1 || rej[0].ErrCode != "0e" || rej[0].ErrSource != "client" || rej[0].Action != "offer" || rej[0].ID != "sig1" {
		t.Fatalf("rejections %+v", rej)
	}
	for _, c := range p.ProbeTargets() {
		if c.Address == "10.87.0.2" {
			t.Fatalf("probe targets took our own candidate from the error: %+v", p.ProbeTargets())
		}
	}
	if got := p.ProbeTargets(); len(got) != 3 || got[0].Address != "37.228.205.125" || got[2].Address != "10.87.0.11" {
		t.Fatalf("probe targets %+v", got)
	}
}

// The fix: addressed by its own NpId (from the room owner), the console finds the connection it activated (own NpId ptnpps3,
// vsh FUN_001e6c40), our offer with psid = its sid passes FUN_001eba64, and the exchange goes on to accept and activated.
func TestMC3ConsoleInitiatedFixed(t *testing.T) {
	p := mc3Participant()
	if err := p.SetHostNpID(consoleNpID()); err != nil {
		t.Fatal(err)
	}
	sent := recorder(p)
	feed(t, p, fromHost(mc3HostOffer))
	host := "PS3Test@pt.np.np.playstation.net/ps3"
	fix := func(s string) string { return strings.ReplaceAll(s, "PS3Test@pt.pt.np.playstation.net/ps3", host) }
	wantSent(t, sent, fix(mc3OurAck), fix(fromUs(mc3OurOffer)))
	if p.HostJID != host {
		t.Fatalf("host JID %s replaced by the stamped from", p.HostJID)
	}
	// probes now carry the console's own NpId as our peer
	_, peer, err := p.NpIDs()
	if err != nil || !bytes.Equal(peer[:24], consoleNpID()) || peer[24] != 1 {
		t.Fatalf("peer NpId %x %v", peer, err)
	}
	// its ack of our offer, the probe phase, then accepts both ways and activated (the mc2 shapes, roles as in vsh states 7-10)
	feed(t, p, `<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" from="PS3Test@pt.pt.np.playstation.net/ps3" type="result" id="sig1"></iq>`)
	wantSent(t, sent)
	if err := p.ProbeOK(); err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, `<iq type="set" id="sig2" to="`+host+`" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="accept" sid="14471" psid="48959" skey="`+ourKey+`"><candidate type="stun" address="10.87.0.2" port="3658" maddress="10.87.0.2" mport="3658"/></query></iq>`)
	feed(t, p, fromHost(`<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" type="set" id="ps3-b3d79762" from="PS3Test@pt.pt.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="accept" sid="48959" psid="14471" skey="SKEY"><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="stun" address="10.87.0.2" port="3658" maddress="10.87.0.11" mport="3658"></candidate></query></iq>`))
	wantSent(t, sent,
		`<iq type="result" id="ps3-b3d79762" to="`+host+`" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p"/></iq>`,
		`<iq type="set" id="sig3" to="`+host+`" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="activated" sid="14471" psid="48959"/></iq>`)
	if c := p.HostCandidate(); c.Address != "10.87.0.11" || c.Port != 3658 {
		t.Fatalf("host candidate %+v", c)
	}
}

// Simultaneous open (two consoles activating toward each other, or our +45 s offer crossing the console's): our offer went out
// with psid 0, the console's comes with psid 0 too. vsh takes that through FUN_001e6d64 (find by NpIds, peer sid 0 or equal);
// we must store its sid, ack, and not offer twice.
func TestSimultaneousOpen(t *testing.T) {
	p := mc3Participant()
	sent := recorder(p)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	*sent = nil
	feed(t, p, fromHost(mc3HostOffer))
	wantSent(t, sent, mc3OurAck)
	if p.peerSID != 48959 {
		t.Fatalf("peer sid %d", p.peerSID)
	}
}

// mc2, 2026-11-11 15:22:05Z: we offer first (psid 0), the PS3 answers with its own offer psid=<our sid>.
var (
	mc2OurOffer   = `<iq type="set" id="sig1" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="offer" sid="976" psid="0" skey="SKEY" nattype="1"><candidate type="stun" address="10.87.0.2" port="3658"/><candidate type="static" address="0.0.0.0" port="3658"/><candidate type="local" address="10.87.0.2" port="3658"/></query></iq>`
	mc2HostOffer  = `<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" type="set" id="ps3-b3d79752" from="PS3Test@pt.pt.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="offer" sid="48955" psid="976" skey="SKEY" nattype="2"><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="stun" address="37.228.205.125" port="5332"></candidate><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="static" address="0.0.0.0" port="3658"></candidate><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="local" address="10.87.0.11" port="3658"></candidate></query></iq>`
	mc2OurAck     = `<iq type="result" id="ps3-b3d79752" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p"/></iq>`
	mc2OurAccept  = `<iq type="set" id="sig2" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="accept" sid="976" psid="48955" skey="SKEY"><candidate type="stun" address="10.87.0.2" port="3658" maddress="10.87.0.2" mport="3658"/></query></iq>`
	mc2HostAccept = `<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" type="set" id="ps3-b3d79753" from="PS3Test@pt.pt.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="accept" sid="48955" psid="976" skey="SKEY"><candidate xmlns="http://playstation.net/np/rt/udpp2p" type="stun" address="10.87.0.2" port="3658" maddress="10.87.0.11" mport="3658"></candidate></query></iq>`
	mc2OurAck2    = `<iq type="result" id="ps3-b3d79753" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p"/></iq>`
	mc2Activated  = `<iq type="set" id="sig3" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="activated" sid="976" psid="48955"/></iq>`
	mc2Terminate  = `<iq xmlns="jabber:client" to="SwitchTest@gb.gb.np.playstation.net/ps3" type="set" id="ps3-b3d79754" from="PS3Test@pt.pt.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p" action="terminate" sid="48955" psid="976"></query></iq>`
	mc2OurAck3    = `<iq type="result" id="ps3-b3d79754" to="PS3Test@pt.pt.np.playstation.net/ps3" from="SwitchTest@gb.gb.np.playstation.net/ps3"><query xmlns="http://playstation.net/np/rt/udpp2p"/></iq>`
)

func TestMC2ProbeInitiated(t *testing.T) {
	p := mc3Participant()
	p.SID = 976
	sent := recorder(p)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, fromUs(mc2OurOffer))
	feed(t, p, fromHost(mc2HostOffer))
	wantSent(t, sent, mc2OurAck)
	if err := p.ProbeOK(); err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, fromUs(mc2OurAccept))
	feed(t, p, fromHost(mc2HostAccept))
	wantSent(t, sent, mc2OurAck2, mc2Activated)
	feed(t, p, mc2Terminate)
	wantSent(t, sent, mc2OurAck3)
}

func TestJIDFromNpID(t *testing.T) {
	j, err := JIDFromNpID(consoleNpID())
	if err != nil || j != "PS3Test@pt.np.np.playstation.net/ps3" {
		t.Fatal(j, err)
	}
	back, _ := NpIDFromJID(j)
	if !bytes.Equal(back[:24], consoleNpID()) {
		t.Fatalf("%x", back)
	}
	if _, err := JIDFromNpID(make([]byte, 24)); err == nil {
		t.Fatal("empty NpId accepted")
	}
}
