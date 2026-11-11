package directory

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The real PS3 01.84 host's session data (ps3gold.go) reads as a lone friends-only host named PS3Test, and the codec writes the same 184
// bytes back from those settings: the mapping changes nothing it should not.
func TestPS3SessionDataGolden(t *testing.T) {
	gold := goldExternal()
	s, players, ok := PS3Codec{}.Settings(gold)
	if !ok || s.HostName != "PS3Test" || s.Visibility != Friends || players != 1 {
		t.Fatalf("read %+v players=%d ok=%v", s, players, ok)
	}
	want := append([]byte{}, gold...) // the 14:12 capture had the first name copy blank; a real host writes it (14:58 capture), so do we
	copy(want[0x02:], "PS3Test")
	want[0x13] = 0x80
	if got := (PS3Codec{}).External(s, players); !bytes.Equal(got, want) {
		t.Fatalf("round trip differs:\n got %x\nwant %x", got, want)
	}
	// the net version is the PS3's, whatever came in
	if _, _, ok := (PS3Codec{}).Settings(append([]byte{0x08, 0x81}, gold[2:]...)); ok {
		t.Fatal("a Wii U net version was accepted as PS3 session data")
	}
}

// The Wii U host row of the 2026-11-07 capture (nex world_consts.go) reads as host OpenPak and is rewritten byte for byte; a different
// name, count and visibility land in the fields the capture shows them in.
func TestWiiUBufferGolden(t *testing.T) {
	gold, _ := hex.DecodeString(wiiuTemplateHex)
	s, players, ok := WiiUCodec{}.Settings(gold)
	if !ok || s.HostName != "OpenPak" || s.Visibility != Friends || players != 2 {
		t.Fatalf("read %+v players=%d ok=%v", s, players, ok)
	}
	if got := (WiiUCodec{}).Buffer(s, players); !bytes.Equal(got, gold) {
		t.Fatalf("round trip differs:\n got %x\nwant %x", got, gold)
	}
	b := WiiUCodec{}.Buffer(Settings{HostName: "Alex", Visibility: FriendsOfFriend}, 3)
	if b[wiiuVis] != 0x8f || b[wiiuPlayers] != 3 || cstr(b[wiiuName2:]) != "Alex" || cstr(b[wiiuName1:]) != "Alex" {
		t.Fatalf("fields: %x", b[:0x20])
	}
	if b[0] != 0x08 || b[1] != 0x81 {
		t.Fatal("net version must stay the Wii U's")
	}
}

// A game crosses the bridge in both directions with its name, visibility and player count, and each side gets its own net version.
func TestSettingsCrossTheBridge(t *testing.T) {
	in := Settings{HostName: "Alexandra", Visibility: FriendsOfFriend}
	ext := PS3Codec{}.External(in, 3)
	got, players, _ := PS3Codec{}.Settings(ext)
	if got != in || players != 3 || ext[0] != 0x08 || ext[1] != 0x84 {
		t.Fatalf("wiiu->ps3: %+v %d %x", got, players, ext[:2])
	}
	buf := WiiUCodec{}.Buffer(got, players)
	back, p2, _ := WiiUCodec{}.Settings(buf)
	if back != in || p2 != 3 || buf[1] != 0x81 {
		t.Fatalf("ps3->wiiu: %+v %d %x", back, p2, buf[:2])
	}
	// a name longer than the field is cut, never run over its neighbour
	long := PS3Codec{}.External(Settings{HostName: "abcdefghijklmnopqrstuvwxyz"}, 1)
	if long[ps3Name+ps3NameLen-1] != 0 || long[ps3Name+ps3NameLen] != goldExternal()[ps3Name+ps3NameLen] {
		t.Fatal("name overran its field")
	}
}

func TestPS3TableAndPresence(t *testing.T) {
	s := Settings{HostName: "PS3Test", Visibility: Friends}
	tab := PS3Codec{}.Table(s, "acct-a")
	gold := goldTable()
	if len(tab) != 256 || !bytes.Equal(tab[:0x14], gold[:0x14]) || !bytes.Equal(tab[0x17:], gold[0x17:]) {
		t.Fatal("table changed outside the per-player bytes")
	}
	if bytes.Equal(tab, PS3Codec{}.Table(s, "acct-b")) {
		t.Fatal("two hosts got the same player bytes")
	}
	p := PS3Codec{}.Presence(s, 0x0001aabbccddeeff, goldExternal())
	if len(p) != 56 || p[0x18] != 0x00 || p[0x19] != 0x01 || p[0x1f] != 0xff || p[0x28] != 0x08 || p[0x29] != 0x84 || p[0x2b] != 0 || p[0x2c] != 1 || p[0x2d] != 0x0f {
		t.Fatalf("presence %x", p)
	}
}

// The presence blob built from the real host's session data is the real host's blob (owner's PS3, 2026-11-11 14:58, scratch/ps3-signaling-2026-11-09/mc1.log),
// except the room id, which the real blob does not carry (see Presence).
func TestPS3PresenceEqualsRealBlob(t *testing.T) {
	const realExt = "088450533354657374000000000000000000008050533354657374000000000000000000008000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000003e9c00000812000101011f00000000000000"
	const realBlob = "5053335465737400000000000000000000800000000000000001000100000001000100000000081208840000010f00000000000000000000"
	ext, _ := hex.DecodeString(realExt)
	if len(ext) != ps3ExtLen {
		t.Fatal("bad golden", len(ext))
	}
	got := PS3Codec{}.Presence(Settings{HostName: "PS3Test"}, 0x0001000100000001, ext)
	if hex.EncodeToString(got) != realBlob {
		t.Fatalf("presence blob\n got %x\nwant %s", got, realBlob)
	}
	// our own External for that host writes the name where the real host does, so the blob a bridged host gets starts with its name + 0x80
	own := PS3Codec{}.External(Settings{HostName: "PS3Test"}, 1)
	if !bytes.Equal(own[2:0x14], ext[2:0x14]) || !bytes.Equal(PS3Codec{}.Presence(Settings{HostName: "PS3Test"}, 1, own)[:18], got[:18]) {
		t.Fatalf("name fields: %x vs %x", own[:0x28], ext[:0x28])
	}
}
