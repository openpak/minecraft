package rtsig

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// STUNMapped discovers the NAT-mapped endpoint of an already-bound UDP socket with one RFC 5389
// binding request, so the offered stun candidate names the endpoint probes will actually come from
// (a console learns it the same way during signaling activation).
func STUNMapped(conn *net.UDPConn, server string) (Candidate, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return Candidate{}, err
	}
	var tid [12]byte
	rand.Read(tid[:])
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001)
	binary.BigEndian.PutUint32(req[4:], 0x2112A442)
	copy(req[8:], tid[:])
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := conn.WriteToUDP(req, srv); err != nil {
			return Candidate{}, err
		}
		conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		buf := make([]byte, 1024)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if time.Now().After(deadline) {
				return Candidate{}, fmt.Errorf("rtsig: stun %s: %w", server, err)
			}
			continue
		}
		if c, ok := ParseMapped(buf[:n], tid); ok {
			conn.SetReadDeadline(time.Time{})
			return c, nil
		}
	}
}

// ParseMapped extracts the mapped endpoint from a STUN binding response.
func ParseMapped(msg []byte, tid [12]byte) (Candidate, bool) {
	if len(msg) < 20 || binary.BigEndian.Uint16(msg[0:]) != 0x0101 || !equal(msg[8:20], tid[:]) {
		return Candidate{}, false
	}
	cookie := binary.BigEndian.Uint32(msg[4:])
	for i := 20; i+4 <= len(msg); {
		typ, ln := int(binary.BigEndian.Uint16(msg[i:])), int(binary.BigEndian.Uint16(msg[i+2:]))
		v := msg[i+4:]
		if ln > len(v) {
			break
		}
		v = v[:ln]
		if (typ == 0x0020 || typ == 0x0001) && len(v) >= 8 && v[1] == 1 {
			port := binary.BigEndian.Uint16(v[2:])
			ip := append([]byte(nil), v[4:8]...)
			if typ == 0x0020 {
				port ^= 0x2112
				for j := range ip {
					ip[j] ^= byte(cookie >> (24 - 8*j))
				}
			}
			return Candidate{Type: "stun", Address: net.IP(ip).String(), Port: port}, true
		}
		i += 4 + (ln+3)&^3
	}
	return Candidate{}, false
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
