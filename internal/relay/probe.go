package relay

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/openpak/minecraft/internal/lcechunk"
	"github.com/openpak/minecraft/internal/pia5"
)

// ProbeResult is what a probe joiner saw.
type ProbeResult struct {
	Messages, Chunks int
	Kinds            []string
	Bedrock          bool // the first decoded chunk has bedrock on its bottom layer
}

// Probe joins as a console would (the capture's joiner messages) and counts what the host sends: the join order of
// docs/provenance.md G3 and chunks that decode. It is the test of the whole stack without a console.
func Probe(ctx context.Context, host netip.AddrPort, gathering uint32, hostPID uint32, key []byte, name string, wantChunks int, wait time.Duration) (ProbeResult, error) {
	var res ProbeResult
	cipher, err := pia5.NewCipher(key, gathering)
	if err != nil {
		return res, err
	}
	jc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return res, err
	}
	self := netip.AddrPortFrom(host.Addr(), uint16(jc.LocalAddr().(*net.UDPAddr).Port))
	if host.Addr().IsLoopback() {
		self = netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", jc.LocalAddr().(*net.UDPAddr).Port))
	}
	var mu sync.Mutex
	var got [][]byte
	j := &pia5.Joiner{Conn: jc, Host: host, Cipher: cipher, PID: 1570020199, HostPID: hostPID, Self: self, Joined: make(chan struct{})}
	j.OnGame = func(b []byte) {
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go j.Run(ctx)
	select {
	case <-j.Joined:
	case <-time.After(15 * time.Second):
		return res, fmt.Errorf("the mesh did not come up (no station handshake answer)")
	case <-ctx.Done():
		return res, ctx.Err()
	}
	hello := make([]byte, 257)
	hello[0], hello[4] = 0xFE, 1
	copy(hello[5:25], "probe-joiner-id-0001")
	copy(hello[85:], name)
	j.SendGame(hello)
	pre := []byte{0, 1, 0xE8, 0x08, 0x81}
	pre = append(pre, 0, byte(len(name)))
	for _, r := range name {
		pre = append(pre, 0, byte(r))
	}
	j.SendGame(pre)
	j.SendGame([]byte{0, 1, 0x01, 0, 0, 1, 0x3c})
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		n := 0
		for _, m := range got {
			if len(m) > 25 && m[0] == 0 && m[1] == 2 && m[2] == 0x33 {
				n++
			}
		}
		mu.Unlock()
		if n >= wantChunks {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	res.Messages = len(got)
	for _, m := range got {
		switch {
		case m[0] == 0xFF:
			res.Kinds = append(res.Kinds, "FF hello")
		case len(m) > 2 && m[0] == 0 && m[1] == 1 && m[2] == 2:
			res.Kinds = append(res.Kinds, "pre-login reply")
		case len(m) == 195:
			res.Kinds = append(res.Kinds, "login response")
		case len(m) > 25 && m[0] == 0 && m[1] == 2 && m[2] == 0x33:
			res.Chunks++
			if !res.Bedrock {
				size := int(binary.BigEndian.Uint32(m[17:21]) & 0x3fffffff)
				if 21+size <= len(m) {
					if c, err := lcechunk.Unpack(m[21 : 21+size]); err == nil && c.Block[lcechunk.Idx(3, 0, 3)]>>4 == 7 {
						res.Bedrock = true
					}
				}
			}
		}
	}
	return res, nil
}
