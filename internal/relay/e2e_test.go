package relay

import (
	"context"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// TestJoinerEndToEnd runs the whole path in one process: a PIA joiner built from the captured joiner's messages, the relay's host,
// the console session and a real Java 1.8.9 server (MC_JAVA_ADDR). It checks the join order of docs/provenance.md G3 and that the
// chunks the "console" receives decode with the capture-derived codec.
func TestJoinerEndToEnd(t *testing.T) {
	java := os.Getenv("MC_JAVA_ADDR")
	if java == "" {
		t.Skip("MC_JAVA_ADDR not set")
	}
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	srv := netip.MustParseAddrPort(conn.LocalAddr().String())
	r := &Relay{Conn: conn, Public: srv, JavaAddr: java, HostName: "OpenPak", ViewDistance: 3}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Serve(ctx)
	key := make([]byte, 32)
	copy(key, "0123456789abcdef0123456789abcdef")
	if _, err := r.Register(ctx, LegSpec{Gathering: 77, HostPID: 2000000001, SessionKey: hex.EncodeToString(key)}); err != nil {
		t.Fatal(err)
	}
	res, err := Probe(ctx, srv, 77, 2000000001, key, "ProbeBot", 10, 40*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", res)
	if res.Chunks < 10 || !res.Bedrock || len(res.Kinds) < 3 {
		t.Fatalf("incomplete join: %+v", res)
	}
}
