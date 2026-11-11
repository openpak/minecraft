package ps3join

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Recorder writes everything the probe sees to <dir>/<label>.log (full hex, nothing truncated), the game bytes in each direction to
// <label>.rx.bin and <label>.tx.bin (the host's stream is what internal/lce/walk reads), and the two UDP skeys to <label>.keys (0600; the
// .log shows stanzas with the skeys masked). Lines go to stdout too, long hex cut at 120 characters.
type Recorder struct {
	mu       sync.Mutex
	log      *os.File
	rx, tx   *os.File
	start    time.Time
	Prefix   string
	skeyMask *regexp.Regexp
}

// NewRecorder creates the files; dir is made if missing.
func NewRecorder(dir, label string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	open := func(ext string) (*os.File, error) {
		return os.OpenFile(filepath.Join(dir, label+ext), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	}
	r := &Recorder{start: time.Now(), skeyMask: regexp.MustCompile(`skey="[0-9a-fA-F]{32}"`)}
	var err error
	if r.log, err = open(".log"); err != nil {
		return nil, err
	}
	if r.rx, err = open(".rx.bin"); err != nil {
		return nil, err
	}
	if r.tx, err = open(".tx.bin"); err != nil {
		return nil, err
	}
	return r, nil
}

// Close ends the files.
func (r *Recorder) Close() { r.log.Close(); r.rx.Close(); r.tx.Close() }

// Logf records one line: tag, then the message.
func (r *Recorder) Logf(tag, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	msg = r.skeyMask.ReplaceAllString(msg, `skey="<masked>"`)
	line := fmt.Sprintf("%s %-8s %s", time.Now().UTC().Format("15:04:05.000"), tag, msg)
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintln(r.log, line)
	if len(line) > 200 {
		line = line[:200] + fmt.Sprintf("... (%d chars, full line in the log file)", len(line))
	}
	fmt.Println(line)
}

// Hex is the hex of b; a hex dump of a game payload goes through Dump.
func Hex(b []byte) string { return hex.EncodeToString(b) }

// Dump records b as a hex dump, 16 bytes a row with an ASCII column, in the log file only.
func (r *Recorder) Dump(tag, what string, b []byte) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %-8s %s (%d bytes)\n", time.Now().UTC().Format("15:04:05.000"), tag, what, len(b))
	for i := 0; i < len(b); i += 16 {
		row := b[i:min(i+16, len(b))]
		asc := make([]byte, len(row))
		for j, c := range row {
			asc[j] = '.'
			if c >= 32 && c < 127 {
				asc[j] = c
			}
		}
		fmt.Fprintf(&sb, "    %04x  %-47x  %s\n", i, row, asc)
	}
	r.mu.Lock()
	r.log.WriteString(sb.String())
	r.mu.Unlock()
}

// Game appends game bytes to the stream file of a direction ("rx" from the PS3, "tx" to it).
func (r *Recorder) Game(dir string, b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.rx
	if dir == "tx" {
		f = r.tx
	}
	f.Write(b)
}

// Keys writes the two skeys (hex) to <dir>/<label>.keys with mode 0600; they decrypt the capture's datagrams and are never printed.
func (r *Recorder) Keys(dir, label string, own, peer []byte) {
	s := fmt.Sprintf("own %s\npeer %s\n", hex.EncodeToString(own), hex.EncodeToString(peer))
	os.WriteFile(filepath.Join(dir, label+".keys"), []byte(s), 0o600)
}
