package walk

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestCaptureStreams walks every decoded game stream (game_<dir>_ch0001.bin packets, ch0002.bin chunks) in MC_CAPTURE_DIR and
// its subdirectories, one per recorded session: each must parse to its last byte, and every packet must survive
// WiiU -> PS3 -> WiiU byte for byte (the PS3 form must also walk as PS3).
func TestCaptureStreams(t *testing.T) {
	dir := os.Getenv("MC_CAPTURE_DIR")
	if dir == "" {
		t.Skip("MC_CAPTURE_DIR not set")
	}
	var files []string
	for _, pat := range []string{"game_*_ch000[12].bin", "*/game_*_ch000[12].bin"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Skip("no game_*_ch000[12].bin streams")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		pkts, rest, err := Walk(b, WiiU)
		name, _ := filepath.Rel(dir, f)
		if err != nil || len(rest) != 0 {
			at := len(b) - len(rest)
			t.Errorf("%s: %d packets, stopped at %d of %d: %v (next bytes % x)", name, len(pkts), at, len(b), err, rest[:min(16, len(rest))])
			continue
		}
		var m UIDMap
		ids := map[byte]int{}
		var ps3 []byte
		for _, p := range pkts {
			ids[p.ID]++
			q := Translate(p, WiiU, PS3, &m)
			ps3 = append(ps3, q...)
			pp, err := Next(q, PS3)
			if err != nil || len(pp.Raw) != len(q) {
				t.Fatalf("%s: %#02x does not walk as PS3: %v", name, p.ID, err)
			}
			if back := Translate(pp, PS3, WiiU, &m); !bytes.Equal(back, p.Raw) {
				t.Fatalf("%s: %#02x round trip differs", name, p.ID)
			}
		}
		if _, rest, err := Walk(ps3, PS3); err != nil || len(rest) != 0 {
			t.Errorf("%s: translated stream does not walk as PS3: %v", name, err)
		}
		keys := make([]int, 0, len(ids))
		for k := range ids {
			keys = append(keys, int(k))
		}
		sort.Ints(keys)
		s := ""
		for _, k := range keys {
			s += fmt.Sprintf(" %02x:%d", k, ids[byte(k)])
		}
		t.Logf("%s: %d bytes, %d packets, 0 left over; ids%s", name, len(b), len(pkts), s)
	}
}
