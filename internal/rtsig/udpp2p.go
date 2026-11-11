package rtsig

import "errors"

// lv2 kernel UDPP2P framing (matching2-protocol.md §28; Pier udpp2p.h): every datagram on UDP 3658 starts
// 0xff, 0x80|kind (|0x40 when both vports < 0x100), then source and destination vport (1 byte each when
// compact, else BE u16). The PS3 kernel drops anything that does not start 0xff.
const (
	KindPlain   = 3      // 4-6 are keyed (signed/encrypted) variants, 7-10 kernel unreachable replies
	VPortSignal = 0xfffe // VSH's signaling socket
)

var ErrNotUDPP2P = errors.New("rtsig: not a UDPP2P datagram")

// Frame prefixes payload with the plain-data UDPP2P header.
func Frame(kind byte, src, dst uint16, payload []byte) []byte {
	if src < 0x100 && dst < 0x100 {
		return append([]byte{0xff, 0x80 | 0x40 | kind, byte(src), byte(dst)}, payload...)
	}
	return append([]byte{0xff, 0x80 | kind, byte(src >> 8), byte(src), byte(dst >> 8), byte(dst)}, payload...)
}

// FrameSignal frames a signaling probe (vport 0xfffe both ways): ff 83 ff fe ff fe + payload.
func FrameSignal(payload []byte) []byte { return Frame(KindPlain, VPortSignal, VPortSignal, payload) }

// Unframe validates and strips the header. Kernel unreachable replies (kinds 7-10) carry no vports or payload.
func Unframe(pkt []byte) (kind byte, src, dst uint16, payload []byte, err error) {
	if len(pkt) < 2 || pkt[0] != 0xff || pkt[1]&0x80 == 0 {
		return 0, 0, 0, nil, ErrNotUDPP2P
	}
	kind = pkt[1] & 0x0f
	switch {
	case kind >= 7 && kind <= 10:
		return kind, 0, 0, nil, nil
	case kind < 3 || kind > 6:
		return 0, 0, 0, nil, ErrNotUDPP2P
	case pkt[1]&0x40 != 0:
		if len(pkt) < 4 {
			return 0, 0, 0, nil, ErrNotUDPP2P
		}
		return kind, uint16(pkt[2]), uint16(pkt[3]), pkt[4:], nil
	}
	if len(pkt) < 6 {
		return 0, 0, 0, nil, ErrNotUDPP2P
	}
	return kind, uint16(pkt[2])<<8 | uint16(pkt[3]), uint16(pkt[4])<<8 | uint16(pkt[5]), pkt[6:], nil
}
