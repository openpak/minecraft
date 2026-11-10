package lce

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unhex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// The joiner's messages, checked against the Quay joiner's on 2026-11-07 (game_j2h_chhello-fe, game_j2h_ch0001 messages 0-2)
// with the same name (XboxTest) and host PID (0x5d949b50); only the shared 16 bytes of each PlayerUID are ours.
func TestClientAgainstRecordedJoiner(t *testing.T) {
	var sent [][]byte
	c := &Client{Name: "XboxTest", PID: 1663490424, HostPID: 0x5d949b50, Send: func(m []byte) { sent = append(sent, m) }}
	uid := hex.EncodeToString(c.uid16())
	fe := "fe00000001" + uid + "da62253400000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000058626f785465737400c32e141a6225c003541da01a6225c07d896707000000007d8958a30000000000000000104d0000105300001a5f11f8000000001a622690034956501a622610000000000000000002d75d3800440065006600610075006c007400001a6225f0000000011a6225f803404c201a6226080355ec8c103600001a6226541a6226480356700c0000000000006d80000000005d949b500000000000000000000000005d949b50"
	if got := hex.EncodeToString(c.Hello()); got != fe {
		t.Fatalf("FE hello\n got %s\nwant %s", got, fe)
	}
	// the host's FF hello -> E8 pre-login
	c.OnGame(tmpl("hello_ff.bin"))
	if got := hex.EncodeToString(last(sent)); got != "0001e80881000800580062006f00780054006500730074" {
		t.Fatalf("pre-login %s", got)
	}
	// the host's keep-alive and pre-login reply (one channel 1 message, as the host sent them) -> 01 login
	c.OnGame(append(unhex("00010000000000"), tmpl("prelogin.bin")...))
	login := "0001010000013c000800580062006f007800540065007300740000000000000000000000000000ffffffffffffffff000000" + uid + "d05cfa58" + uid + "d1ca00ff00000000000100000000000000000000000000000000000000000000800100000000000000000000ffffffff"
	if got := hex.EncodeToString(last(sent)); got != login {
		t.Fatalf("login\n got %s\nwant %s", got, login)
	}
	// login response (cut over two messages, as 5,120-byte pieces are) and the host's D6 placing the player (game_h2j packet
	// 217) -> 87 01 + 0D, the joiner's message at 0.468 s
	resp := tmpl("login.bin")
	c.OnGame(append([]byte{0, 1}, resp[:60]...))
	c.OnGame(append(append([]byte{0, 1}, resp[60:]...), unhex("d6c061b000000000004052400000000000407028000000000001000000000000000001")...))
	if got := hex.EncodeToString(last(sent)); got != "000187010dc061b0000000000040524000000000004070280000000000000000000000000000" {
		t.Fatalf("teleport answer %s stats %+v", got, c.Stats())
	}
	st := c.Stats()
	if !st.HostHello || !st.PreLogin || !st.Login || !st.InWorld || st.EntityID != 237 || st.X != -141.5 || st.Y != 73 || st.Z != 258.5 {
		t.Fatalf("stats %+v", st)
	}
	// what the tick loop sends once in the world: E2 keep-alive with the host's token and 0B on the ground
	if got := hex.EncodeToString(keepAlive(0)) + hex.EncodeToString(position(st.X, st.Y, st.Z, true)); got != "e200"+"0bc061b000000000004052400000000000407028000000000001" {
		t.Fatalf("idle packets %s", got)
	}
}

func last(b [][]byte) []byte { return b[len(b)-1] }

// TestClientRecordedHostStream feeds every message the real Wii U host sent on 2026-11-07 (game_h2j_ch0001 and ch0002, in
// message order per channel, MC_CAPTURE_DIR) to the client: it must walk all of channel 1, reach the world and count the chunks.
func TestClientRecordedHostStream(t *testing.T) {
	dir := os.Getenv("MC_CAPTURE_DIR")
	if dir == "" {
		t.Skip("MC_CAPTURE_DIR not set")
	}
	var sent int
	c := &Client{Name: "WiiTest", PID: 1358756791, HostPID: 0x5d949b50, Send: func([]byte) { sent++ }}
	for _, ch := range []byte{1, 2} {
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("game_h2j_ch%04d.bin", ch)))
		if err != nil {
			t.Skip(err)
		}
		idx, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("game_h2j_ch%04d.idx", ch)))
		for _, l := range strings.Split(strings.TrimSpace(string(idx)), "\n") {
			var tm float64
			var o, n int
			fmt.Sscan(l, &tm, &o, &n)
			c.OnGame(append([]byte{0, ch}, b[o:o+n]...))
		}
	}
	st := c.Stats()
	t.Logf("%+v, %d messages sent", st, sent)
	if c.walkErr != nil || len(c.ch1) != 0 || !st.PreLogin || !st.Login || !st.InWorld || st.Chunks != 400 || st.Packets != 15009 {
		t.Fatalf("walk err %v rest %d stats %+v", c.walkErr, len(c.ch1), st)
	}
}
