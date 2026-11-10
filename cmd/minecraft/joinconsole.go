package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/openpak/minecraft/internal/relay"
)

// joinconsole joins a world hosted on a real Wii U as a bridged Wii U joiner and holds the session (prds#177). The NEX side goes
// through nex minecraft-wiiu's internal surface (bearer MINECRAFT_INTERNAL_KEY, from the environment or -keyfile); the session key
// it returns is never printed.
func joinconsole() error {
	fs := flag.NewFlagSet("joinconsole", flag.ExitOnError)
	nexURL := fs.String("nex", env("MINECRAFT_NEX_URL", "http://127.0.0.1:22432"), "nex minecraft-wiiu health listener (internal routes)")
	keyfile := fs.String("keyfile", "", "file holding MINECRAFT_INTERNAL_KEY (default: the environment variable)")
	list := fs.Bool("list", false, "list console-hosted gatherings and exit")
	gid := fs.Uint("gathering", 0, "gathering to join (0: the first listed)")
	pid := fs.Uint("pid", 1358756791, "the joining player's Wii U PID (WiiTest)")
	name := fs.String("name", "", "player name (default: the account's)")
	bind := fs.String("bind", ":0", "local UDP address (the box: 10.0.0.192:0, never an nncs address)")
	public := fs.String("public", "", "address to claim as ours (the box: 145.241.206.67; default: the local address toward the host)")
	hold := fs.Duration("hold", 5*time.Minute+30*time.Second, "how long to stay in the world")
	fs.Parse(os.Args[2:])
	key := os.Getenv("MINECRAFT_INTERNAL_KEY")
	if *keyfile != "" {
		b, err := os.ReadFile(*keyfile)
		if err != nil {
			return err
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		return fmt.Errorf("no internal key: set MINECRAFT_INTERNAL_KEY or -keyfile")
	}
	n := relay.Nex{URL: *nexURL, Key: key}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *list {
		gs, err := n.Gatherings(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(gs)
	}
	cj := relay.ConsoleJoin{Nex: n, Gathering: uint32(*gid), PID: uint32(*pid), Name: *name, Bind: *bind, Hold: *hold}
	if *public != "" {
		a, err := netip.ParseAddr(*public)
		if err != nil {
			return err
		}
		cj.Public = a
	}
	res, err := cj.Run(ctx)
	fmt.Printf("%+v\n", res)
	return err
}
