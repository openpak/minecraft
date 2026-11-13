package bridge

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/ps3join"
)

// Trace is the per-session record of both legs, so the first hardware run can be debugged from files alone (dir/<label>.*):
//
//	.log          every line of both legs: PS3 signalling, RUDP and messages in full hex, and from the bridge every packet by name
//	              ("WALK wiiu->ps3 0x01 ClientboundLogin, 152 bytes -> 160 bytes"), state changes ("STATE ...")
//	.rx.bin       raw game bytes the PS3 host sent us (PS3 dialect, what internal/lce/walk reads)
//	.tx.bin       what we sent the PS3 host: its 32-byte hello and every translated game message (PS3 dialect)
//	.wiiu.rx.bin  raw reliable messages the Wii U sent us, each as u16 length + the message (`00 channel` framing on it)
//	.wiiu.tx.bin  what we sent the Wii U (translated, Wii U dialect), same framing
type Trace struct {
	Rec      *ps3join.Recorder
	wrx, wtx *os.File
	mu       sync.Mutex
}

// NewTrace creates the files of one session.
func NewTrace(dir, label string) (*Trace, error) {
	rec, err := ps3join.NewRecorder(dir, label)
	if err != nil {
		return nil, err
	}
	rec.Quiet = true // the service logs state changes itself; everything else is in the file
	t := &Trace{Rec: rec}
	open := func(ext string) *os.File {
		f, e := os.OpenFile(filepath.Join(dir, label+ext), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if e != nil && err == nil {
			err = e
		}
		return f
	}
	t.wrx, t.wtx = open(".wiiu.rx.bin"), open(".wiiu.tx.bin")
	if err != nil {
		rec.Close()
		return nil, err
	}
	return t, nil
}

// Logf records one line.
func (t *Trace) Logf(tag, format string, a ...any) {
	if t != nil {
		t.Rec.Logf(tag, format, a...)
	}
}

// WiiU records one reliable message of the Wii U leg: dir "rx" (from the console) or "tx".
func (t *Trace) WiiU(dir string, msg []byte) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.wrx
	if dir == "tx" {
		f = t.wtx
	}
	f.Write(binary.BigEndian.AppendUint16(nil, uint16(len(msg))))
	f.Write(msg)
}

// Close ends the files.
func (t *Trace) Close() {
	if t != nil {
		t.Rec.Close()
		t.wrx.Close()
		t.wtx.Close()
	}
}

// Label is the file name prefix of a session: start time, game, joiner PID.
func Label(game string, pid uint32, at time.Time) string {
	g := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, game)
	return fmt.Sprintf("%s-%s-pid%d", at.UTC().Format("20060102-150405"), g, pid)
}
