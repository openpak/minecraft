package rtsig

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Client is one RT stream connection authenticated as a joiner account (design 1 in the decode:
// a real participant connection, so the server's peer routing works in both directions).
type Client struct {
	conn   net.Conn
	dec    *xml.Decoder
	mu     sync.Mutex
	Domain string
	JID    string
	Log    func(string) // if set, called with each incoming stanza
}

// DialRT connects to the RT service. addr is host:port; useTLS wraps the connection (the public
// 5223 front), otherwise plain (the internal listener). auth is the raw NP ticket for SASL.
func DialRT(addr string, useTLS bool, domain string, auth []byte) (*Client, error) {
	var conn net.Conn
	var err error
	if useTLS {
		d := &net.Dialer{Timeout: 10 * time.Second}
		// The RT front is a PS3-era TLS endpoint (old versions and RSA-CBC suites, OpenPak CA):
		// match what the firmware offers and skip verification for this internal participant.
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{
			InsecureSkipVerify: true, ServerName: domain,
			MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			},
		})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, Domain: domain}
	c.dec = xml.NewDecoder(bufio.NewReader(conn))
	open := `<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" to="` + domain + `" version="1.0">`
	if err := c.raw(open); err != nil {
		return nil, err
	}
	if err := c.skipStreamOpen(); err != nil {
		return nil, fmt.Errorf("rtsig: stream open: %w", err)
	}
	if _, err := c.readElement(); err != nil { // features
		return nil, err
	}
	if err := c.raw(`<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="I5R-TICKET-AUTH">` + base64.StdEncoding.EncodeToString(auth) + `</auth>`); err != nil {
		return nil, err
	}
	el, err := c.readElement()
	if err != nil || el != "success" {
		return nil, fmt.Errorf("rtsig: auth failed (%s)", el)
	}
	c.dec = xml.NewDecoder(bufio.NewReader(conn))
	if err := c.raw(open); err != nil {
		return nil, err
	}
	if err := c.skipStreamOpen(); err != nil {
		return nil, err
	}
	if _, err := c.readElement(); err != nil { // features
		return nil, err
	}
	if err := c.raw(`<iq type="set" id="bind"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>ps3</resource></bind></iq>`); err != nil {
		return nil, err
	}
	for i := 0; i < 20; i++ {
		s, err := c.readStanza()
		if err != nil {
			return nil, err
		}
		if strings.Contains(s, `"bind"`) || strings.Contains(s, "<bind") {
			if j := between(s, "<jid>", "</jid>"); j != "" {
				c.JID = j
			}
			break
		}
	}
	return c, nil
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i+len(a):], b)
	if j < 0 {
		return ""
	}
	return s[i+len(a) : i+len(a)+j]
}

func (c *Client) raw(s string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := io.WriteString(c.conn, s)
	return err
}

// Send writes one stanza.
func (c *Client) Send(s string) error { return c.raw(s) }

// skipStreamOpen consumes tokens until the stream:stream start element ends.
func (c *Client) skipStreamOpen() error {
	for {
		t, err := c.dec.Token()
		if err != nil {
			return err
		}
		if se, ok := t.(xml.StartElement); ok && se.Name.Local == "stream" {
			return nil
		}
	}
}

// readElement consumes one top-level element and returns its local name.
func (c *Client) readElement() (string, error) {
	for {
		t, err := c.dec.Token()
		if err != nil {
			return "", err
		}
		if se, ok := t.(xml.StartElement); ok {
			var v struct{}
			if err := c.dec.DecodeElement(&v, &se); err != nil {
				return "", err
			}
			return se.Name.Local, nil
		}
	}
}

// readStanza consumes one top-level element and returns its XML.
func (c *Client) readStanza() (string, error) {
	for {
		t, err := c.dec.Token()
		if err != nil {
			return "", err
		}
		if se, ok := t.(xml.StartElement); ok {
			var raw struct {
				XML string `xml:",innerxml"`
			}
			// Re-encode from the parsed node instead: decode into a generic tree via Parse path.
			var sb strings.Builder
			sb.WriteString("<" + se.Name.Local)
			for _, a := range se.Attr {
				sb.WriteString(" " + a.Name.Local + `="` + a.Value + `"`)
			}
			sb.WriteString(">")
			if err := c.dec.DecodeElement(&raw, &se); err != nil {
				return "", err
			}
			sb.WriteString(raw.XML)
			sb.WriteString("</" + se.Name.Local + ">")
			return sb.String(), nil
		}
	}
}

// Run pumps incoming stanzas into p until the connection fails.
func (c *Client) Run(p *Participant) error {
	for {
		s, err := c.readStanza()
		if err != nil {
			return err
		}
		if c.Log != nil {
			c.Log(s)
		}
		st, err := Parse(s)
		if err != nil {
			continue
		}
		if err := p.Handle(st); err != nil {
			return err
		}
	}
}

// Close ends the stream.
func (c *Client) Close() error { return c.conn.Close() }
