package rtsig

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strings"
)

// The signaling UDP probe, 0x58 bytes (vsh 4.93 builder FUN_001ea168, parser FUN_001e9518):
//
//	+0x00 type: 1 request, 2 response, 3 bandwidth pair; 4/5 are a 16-byte echo (see EchoReply)
//	+0x04 sender's own NpId (32-byte probe form, see NpIDFromJID)
//	+0x24 sender's peer NpId
//	+0x44 sender's sid, +0x46 peer sid
//	+0x48 u64 send time (type 1), echoed by type 2
//	+0x50 type 2: requester's IPv4 as the responder saw it, XOR the u32 at +0x44
//	+0x54 type 2: requester's port, XOR the u16 at +0x44
//	+0x50 type 3: u32 link estimate of the *receiver*, 0x22368b800 / median gap in us of the last 8 type-3
//	      pairs the sender got from it (0 = none yet); then 0x400 random padding bytes (0x458 in all)
//
// Type 3 (FUN_001ea500) is sent only once the connection is established (vsh state 10): two identical
// datagrams back to back, 9 times 500 ms apart, then every ~50 s. Plain, unsigned, no key material; the
// padding only gives the pair its size. The receiver times the gap between the two and answers nothing.
//
// The receiver looks its connection up with sceNpUtilCmpNpId (FUN_001f79bc): packet +0x24 against its own
// NpId, +0x04 against its peer's. The compare fails unless reserved[0] == 1 on both sides and opt[0..4]
// match exactly, so a probe without proper NpIds is dropped silently.
const probeLen = 0x58

// NpIDFromJID returns the probe form of the NpId vsh builds from an RT JID (FUN_001f9024 / FUN_001f8d98):
// online id (16, NUL padded), opt = domain labels 1 and 2 (2 chars each) + resource (<= 4 chars),
// reserved[0] = 1. "SwitchTest@us.us.np.playstation.net/ps3" -> "SwitchTest", "ususps3\0", 01 00...
func NpIDFromJID(jid string) ([]byte, error) {
	node, rest, ok := strings.Cut(jid, "@")
	domain, res, _ := strings.Cut(rest, "/")
	if !ok || len(node) == 0 || len(node) > 16 || len(res) > 4 || len(domain) < 6 || domain[2] != '.' || domain[5] != '.' {
		return nil, errors.New("rtsig: not an NP JID: " + jid)
	}
	b := make([]byte, 32)
	copy(b, node)
	copy(b[16:], domain[0:2])
	copy(b[18:], domain[3:5])
	copy(b[20:], res)
	b[24] = 1
	return b, nil
}

// JIDFromNpID is the inverse of NpIDFromJID for a 24-byte Matching2 NpId (online id 16, opt 8): "PS3Test" + "ptnpps3" ->
// "PS3Test@pt.np.np.playstation.net/ps3". np-community routes on the online id, so any such domain reaches the console.
func JIDFromNpID(npid []byte) (string, error) {
	if len(npid) < 24 {
		return "", errors.New("rtsig: short NpId")
	}
	node := strings.TrimRight(string(npid[:16]), "\x00")
	opt := npid[16:24]
	res := strings.TrimRight(string(opt[4:8]), "\x00")
	for _, c := range opt[:4] {
		if c < 'a' || c > 'z' {
			return "", errors.New("rtsig: NpId has no realm opt")
		}
	}
	if node == "" || res == "" {
		return "", errors.New("rtsig: NpId without online id or resource")
	}
	return node + "@" + string(opt[0:2]) + "." + string(opt[2:4]) + ".np.playstation.net/" + res, nil
}

// ProbePacket builds a type-1 probe from our connection: own NpId at +0x04, peer NpId at +0x24.
func ProbePacket(ownNpID, peerNpID []byte, ownSID, peerSID uint16, now uint64) []byte {
	out := make([]byte, probeLen)
	out[0] = 1
	copy(out[0x04:0x24], ownNpID)
	copy(out[0x24:0x44], peerNpID)
	binary.BigEndian.PutUint16(out[0x44:], ownSID)
	binary.BigEndian.PutUint16(out[0x46:], peerSID)
	binary.BigEndian.PutUint64(out[0x48:], now)
	return out
}

// ProbeResponsePacket answers a type-1 request received from src (vsh event 0x10 path): our NpIds and
// sids, the request's +0x48 time echoed, and src (the requester's mapped endpoint) XORed with our sid pair.
func ProbeResponsePacket(req []byte, src *net.UDPAddr, ownNpID, peerNpID []byte, ownSID, peerSID uint16) []byte {
	ip := src.IP.To4()
	if len(req) < probeLen || req[0] != 1 || ip == nil {
		return nil
	}
	out := ProbePacket(ownNpID, peerNpID, ownSID, peerSID, binary.BigEndian.Uint64(req[0x48:]))
	out[0] = 2
	binary.BigEndian.PutUint32(out[0x50:], binary.BigEndian.Uint32(ip)^(uint32(ownSID)<<16|uint32(peerSID)))
	binary.BigEndian.PutUint16(out[0x54:], uint16(src.Port)^ownSID)
	return out
}

// ProbeFrom reports whether body is a probe the peer sent to us (its online id at +0x04, ours at +0x24).
// Only the online ids are compared: the opt/reserved bytes are whatever the host's connection holds (its
// Matching2 member NpId may differ from our JID form). Our own probes hairpinned back by a shared NAT
// carry our online id at +0x04 and are rejected.
func ProbeFrom(body, ownNpID, peerNpID []byte) bool {
	return len(body) >= probeLen && body[0] >= 1 && body[0] <= 3 &&
		bytes.Equal(body[0x04:0x14], peerNpID[:16]) && bytes.Equal(body[0x24:0x34], ownNpID[:16])
}

// Bandwidth decodes a type-3 bandwidth pair: the sender's estimate of our link (0 until we send pairs).
func Bandwidth(body []byte) (estimate uint32, ok bool) {
	if len(body) < probeLen || body[0] != 3 {
		return 0, false
	}
	return binary.BigEndian.Uint32(body[0x50:]), true
}

// EchoReply answers a type-4 echo (16 bytes: type, pad, u16 request id, u16, u64 send time) the way vsh
// FUN_001e9518 does: the same 16 bytes back with type 5. nil for anything else.
func EchoReply(body []byte) []byte {
	if len(body) < 16 || body[0] != 4 {
		return nil
	}
	out := append([]byte(nil), body[:16]...)
	out[0] = 5
	return out
}

// ProbeMapped decodes a type-2 response: our endpoint as the responder saw it.
func ProbeMapped(resp []byte) *net.UDPAddr {
	if len(resp) < probeLen || resp[0] != 2 {
		return nil
	}
	pair := binary.BigEndian.Uint32(resp[0x44:])
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, binary.BigEndian.Uint32(resp[0x50:])^pair)
	return &net.UDPAddr{IP: ip, Port: int(binary.BigEndian.Uint16(resp[0x54:]) ^ uint16(pair>>16))}
}
