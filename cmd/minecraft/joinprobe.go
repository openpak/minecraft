package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/openpak/minecraft/internal/relay"
)

// joinprobe joins a running service the way a console does and reports what it received. The session key is read from a file
// (hex, mode 0600) and never printed.
func joinprobe() error {
	fs := flag.NewFlagSet("joinprobe", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1:22960", "service address ip:port")
	gid := fs.Uint("gathering", 0, "the world's gathering id")
	pid := fs.Uint("hostpid", 2000000001, "the world's owner PID")
	keyfile := fs.String("keyfile", "", "file holding the session key as hex")
	player := fs.Uint("pid", 1570020199, "the joining player's Wii U PID (a test account's, to appear as that friend)")
	name := fs.String("name", "ProbeBot", "console player name")
	want := fs.Int("chunks", 20, "chunk messages to wait for")
	wait := fs.Duration("wait", 60*time.Second, "how long to stay in the world (it leaves early once -chunks arrived)")
	fs.Parse(os.Args[2:])
	b, err := os.ReadFile(*keyfile)
	if err != nil {
		return err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	ap, err := netip.ParseAddrPort(*host)
	if err != nil {
		return err
	}
	res, err := relay.Probe(context.Background(), ap, uint32(*gid), uint32(*pid), key, uint32(*player), *name, *want, *wait)
	fmt.Printf("%+v\n", res)
	return err
}
