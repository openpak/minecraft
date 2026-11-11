// Package ps3join is the PS3 joiner probe: it joins a Minecraft 01.84 world a real PS3 hosts, the way a PS3 joiner does
// (research/minecraft-lce/ps3-0184-p2p-transport-2026-11-11.md §3.1), and records every byte of what happens.
package ps3join

import (
	"encoding/binary"
	"strings"
)

// Entry is one record of the host's player table (spec §4.2): 8 entries of 32 bytes, entries 0-3 in internal attr 0x57 and 4-7 in 0x58.
type Entry struct {
	UID    [28]byte // the PlayerUID; zero until the host has the player's hello
	Member uint16   // Matching2 member id of the console that owns the pad
	Small  byte     // the small id the host assigned
	Pad    byte     // pad index on that console (high nibble of the last byte)
	Count  byte     // entry 0 only: the low nibble of the last byte, number of entries - 1
	Index  int
}

// ParseTable reads the non-empty entries of 0x57 and 0x58.
func ParseTable(a57, a58 []byte) []Entry {
	var out []Entry
	all := append(pad(a57, 128), pad(a58, 128)...)
	for i := 0; i < 8; i++ {
		r := all[i*32 : i*32+32]
		e := Entry{Member: binary.BigEndian.Uint16(r[0x1c:]), Small: r[0x1e], Pad: r[0x1f] >> 4, Count: r[0x1f] & 15, Index: i}
		copy(e.UID[:], r[:28])
		if e.Member == 0 && e.UID == [28]byte{} && i > 0 {
			continue
		}
		out = append(out, e)
	}
	return out
}

func pad(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

// Find returns the entry the host wrote for a member id (not entry 0, which is the host itself, unless it is the only match).
func Find(es []Entry, member uint16) (Entry, bool) {
	for _, e := range es {
		if e.Member == member && e.Index != 0 {
			return e, true
		}
	}
	return Entry{}, false
}

// Name is the online id characters of an entry's UID.
func (e Entry) Name() string { return strings.TrimRight(string(e.UID[:16]), "\x00") }

// PlayerUID builds the 28-byte PlayerUID of spec §5: 16 online id characters, the terminator, flags (bit 7 signed in, bits 6-5 pad), the
// 6-byte console MAC and the u32 user id.
func PlayerUID(onlineID string, padIdx int, mac [6]byte, userID uint32) (uid [28]byte) {
	copy(uid[:16], onlineID)
	uid[0x11] = 0x80 | byte(padIdx&3)<<5
	copy(uid[0x12:], mac[:])
	binary.BigEndian.PutUint32(uid[0x18:], userID)
	return
}

// Hello is the 32-byte first message of a joiner (§3.1 step 9): the small id, three bytes the constructor does not write (zero: a guess,
// ps3p2p.GuessHelloPad) and the joiner's PlayerUID.
func Hello(small byte, uid [28]byte) []byte {
	b := make([]byte, 32)
	b[0] = small
	copy(b[4:], uid[:])
	return b
}
