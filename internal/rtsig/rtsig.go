// Copied from common/terraria/internal/rtsig (github.com/openpak/terraria; the Terraria signalprobe is verified byte for byte against
// a real PS3). Copied rather than shared: both are internal packages of two modules, and np-common is not a dependency of this one.
// Only change: Participant.PeerSKey and HostCandidate. Keep in step by hand until the pieces move into np-common.

// Package rtsig is the NP RT signaling exchange (udpp2p/netinfo) a relay peer speaks to a PS3 host so the
// host's game reaches EXT_MUTUAL_ACTIVATED and initiates cellRudp (research/ps3/matching2-protocol.md §24-25).
// Stanza shapes are the firmware's (vsh SceNpSignaling): iq stanzas, everything in attributes.
package rtsig

import (
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

const (
	NSUdpp2p  = "http://playstation.net/np/rt/udpp2p"
	NSNetinfo = "http://playstation.net/np/rt/netinfo"
)

// Candidate is one peer endpoint candidate in an offer/accept.
type Candidate struct {
	Type     string // local, derived, stun, static
	Address  string
	Port     uint16
	MAddress string // accept only (mandatory there)
	MPort    uint16
}

// Stanza is one parsed signaling iq.
type Stanza struct {
	Kind         string // get, set, result, error
	ID, From, To string
	NS           string
	Action       string // offer, accept, terminate, activated, deactivated, fterminate (udpp2p)
	SID, PSID    uint16
	SKey         []byte
	NATType      uint16
	Candidates   []Candidate
	// netinfo result fields
	NetLocal, NetMapped string
	NetUPnP             string
	NetNPStatus         string
	NetNPPort           uint16
	NetNAT              string
}

type xmlQuery struct {
	XMLName    xml.Name `xml:"query"`
	NS         string   `xml:"xmlns,attr"`
	Action     string   `xml:"action,attr"`
	SID        string   `xml:"sid,attr"`
	PSID       string   `xml:"psid,attr"`
	SKey       string   `xml:"skey,attr"`
	NATType    string   `xml:"nattype,attr"`
	Candidates []struct {
		Type     string `xml:"type,attr"`
		Address  string `xml:"address,attr"`
		Port     string `xml:"port,attr"`
		MAddress string `xml:"maddress,attr"`
		MPort    string `xml:"mport,attr"`
	} `xml:"candidate"`
	Children []struct {
		XMLName xml.Name
		Local   string `xml:"local,attr"`
		Mapped  string `xml:"mapped,attr"`
		Status  string `xml:"status,attr"`
		Port    string `xml:"port,attr"`
	} `xml:",any"`
}

type xmlIQ struct {
	XMLName xml.Name  `xml:"iq"`
	Kind    string    `xml:"type,attr"`
	ID      string    `xml:"id,attr"`
	From    string    `xml:"from,attr"`
	To      string    `xml:"to,attr"`
	Query   *xmlQuery `xml:"query"`
}

// Parse reads one signaling iq stanza.
func Parse(s string) (Stanza, error) {
	var iq xmlIQ
	if err := xml.Unmarshal([]byte(s), &iq); err != nil {
		return Stanza{}, err
	}
	if iq.Query == nil {
		return Stanza{Kind: iq.Kind, ID: iq.ID, From: iq.From, To: iq.To}, nil
	}
	st := Stanza{Kind: iq.Kind, ID: iq.ID, From: iq.From, To: iq.To, NS: iq.Query.NS, Action: iq.Query.Action}
	u16 := func(v string) uint16 { n, _ := strconv.ParseUint(v, 10, 16); return uint16(n) }
	st.SID, st.PSID, st.NATType = u16(iq.Query.SID), u16(iq.Query.PSID), u16(iq.Query.NATType)
	if iq.Query.SKey != "" {
		k, err := hex.DecodeString(iq.Query.SKey)
		if err != nil || len(k) != 16 {
			return Stanza{}, fmt.Errorf("rtsig: bad skey")
		}
		st.SKey = k
	}
	for _, c := range iq.Query.Candidates {
		st.Candidates = append(st.Candidates, Candidate{Type: c.Type, Address: c.Address, Port: u16(c.Port), MAddress: c.MAddress, MPort: u16(c.MPort)})
	}
	for _, ch := range iq.Query.Children {
		switch ch.XMLName.Local {
		case "address":
			st.NetLocal, st.NetMapped = ch.Local, ch.Mapped
		case "upnp":
			st.NetUPnP = ch.Status
		case "npport":
			st.NetNPStatus, st.NetNPPort = ch.Status, u16(ch.Port)
		case "nat":
			st.NetNAT = ch.Status
		}
	}
	return st, nil
}

func esc(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func iqXML(kind, id, to, from, inner string) string {
	s := `<iq type="` + kind + `"`
	if id != "" {
		s += ` id="` + esc(id) + `"`
	}
	if to != "" {
		s += ` to="` + esc(to) + `"`
	}
	if from != "" {
		s += ` from="` + esc(from) + `"`
	}
	if inner == "" {
		return s + `></iq>`
	}
	return s + `>` + inner + `</iq>`
}

func candidatesXML(cs []Candidate, mapped bool) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(`<candidate type="` + esc(c.Type) + `" address="` + esc(c.Address) + `" port="` + strconv.Itoa(int(c.Port)) + `"`)
		if mapped {
			b.WriteString(` maddress="` + esc(c.MAddress) + `" mport="` + strconv.Itoa(int(c.MPort)) + `"`)
		}
		b.WriteString(`/>`)
	}
	return b.String()
}

// Offer builds an udpp2p offer query stanza (iq type=set).
func Offer(id, to, from string, sid, psid uint16, skey []byte, nat uint16, cs []Candidate) string {
	q := `<query xmlns="` + NSUdpp2p + `" action="offer" sid="` + strconv.Itoa(int(sid)) + `" psid="` + strconv.Itoa(int(psid)) +
		`" skey="` + hex.EncodeToString(skey) + `" nattype="` + strconv.Itoa(int(nat)) + `">` + candidatesXML(cs, false) + `</query>`
	return iqXML("set", id, to, from, q)
}

// Accept builds an udpp2p accept (candidates must carry mapped address/port; the firmware requires them).
func Accept(id, to, from string, sid, psid uint16, skey []byte, cs []Candidate) string {
	q := `<query xmlns="` + NSUdpp2p + `" action="accept" sid="` + strconv.Itoa(int(sid)) + `" psid="` + strconv.Itoa(int(psid)) +
		`" skey="` + hex.EncodeToString(skey) + `">` + candidatesXML(cs, true) + `</query>`
	return iqXML("set", id, to, from, q)
}

// Simple builds an activated/deactivated/terminate stanza (attributes only, no skey/candidates).
func Simple(action, id, to, from string, sid, psid uint16) string {
	q := `<query xmlns="` + NSUdpp2p + `" action="` + action + `" sid="` + strconv.Itoa(int(sid)) + `" psid="` + strconv.Itoa(int(psid)) + `"/>`
	return iqXML("set", id, to, from, q)
}

// Ack answers a udpp2p set. The firmware's own ack is a bare iq result, but np-community (<= v0.4.8)
// only forwards iqs that carry a udpp2p/netinfo query child and silently drops bare results, so the
// host never saw our ack of its offer, stayed in state 5 (own offer unacked) and never entered the
// probing state 7 (vsh FUN_001f5d78 event 9). An empty udpp2p query rides through the relay; the
// host matches results by iq id (it has no action, so its stanza parser never runs on it).
// ponytail: workaround for the relay; drop the query once np-community forwards bare results.
func Ack(id, to, from string) string {
	return iqXML("result", id, to, from, `<query xmlns="`+NSUdpp2p+`"/>`)
}

// NetinfoResult answers a netinfo get with the four required child groups in firmware order.
func NetinfoResult(id, to, from, local, mapped, upnp, npStatus string, npPort uint16, nat string) string {
	q := `<query xmlns="` + NSNetinfo + `"><address local="` + esc(local) + `" mapped="` + esc(mapped) + `"/><upnp status="` + esc(upnp) +
		`"/><npport status="` + esc(npStatus) + `" port="` + strconv.Itoa(int(npPort)) + `"/><nat status="` + esc(nat) + `"/></query>`
	return iqXML("result", id, to, from, q)
}

// NetinfoGet asks a peer for its netinfo.
func NetinfoGet(id, to, from string) string {
	return iqXML("get", id, to, from, `<query xmlns="`+NSNetinfo+`"/>`)
}
