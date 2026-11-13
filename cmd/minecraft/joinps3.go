package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/openpak/minecraft/internal/ps3join"
)

// joinps3 joins a Minecraft 01.84 world a real PS3 hosts, as a second PS3 would (prds#177, research/minecraft-lce/ps3-0184-p2p-transport-2026-11-11.md
// §3.1), and records everything to -out: <label>.log (every datagram raw and decrypted, every RUDP frame and message, hex dumps), <label>.rx.bin
// and .tx.bin (the game bytes each way), <label>.keys (the two UDP skeys, 0600). It prints how far it got as one SUMMARY line. Run it through
// scratch/ps3-signaling-2026-11-09/run-minecraft.sh, which also captures the packets and fetches the ticket and the title file.
func joinps3() error {
	fs := flag.NewFlagSet("joinps3", flag.ExitOnError)
	var c ps3join.Config
	fs.StringVar(&c.OutDir, "out", "ps3join-out", "directory for the recording")
	fs.StringVar(&c.Label, "label", "run", "file name prefix of the recording")
	fs.StringVar(&c.RT, "rt", env("MC_PS3_RT", "127.0.0.1:8523"), "np-community RT address")
	fs.BoolVar(&c.RTTLS, "rt-tls", env("MC_PS3_RT_TLS", "") == "1", "RT over TLS (the public 5223 front)")
	fs.StringVar(&c.Domain, "domain", env("MC_PS3_DOMAIN", "us.us.np.playstation.net"), "NP domain of the probe account (<cc>.<cc>.np.playstation.net)")
	fs.StringVar(&c.TicketFile, "ticket-file", os.Getenv("MC_PS3_TICKET_FILE"), "NP ticket of the probe account (np-matching2 `ticket <online id>`)")
	fs.StringVar(&c.OnlineID, "account", env("MC_PS3_ACCOUNT", "SwitchTest"), "the probe account's online id")
	fs.StringVar(&c.Country, "country", "", "ticket country (default: the domain's first label)")
	fs.StringVar(&c.HostID, "host", os.Getenv("MC_PS3_HOST"), "online id of the PS3 hosting the world")
	fs.StringVar(&c.HostJID, "host-jid", os.Getenv("MC_PS3_HOST_JID"), "the host's RT JID (default <host>@<domain>/ps3)")
	fs.StringVar(&c.M2Plain, "m2-plain", env("MC_PS3_M2_PLAIN", "145.241.199.19:3480"), "np-matching2 plain endpoint")
	fs.StringVar(&c.M2TLS, "m2-tls", env("MC_PS3_M2_TLS", "145.241.199.19:443"), "np-matching2 TLS endpoint (the firmware's port 443)")
	fs.StringVar(&c.M2Name, "m2-name", "matching.np.community.playstation.net", "the hostname the TLS front routes the agent on")
	fs.StringVar(&c.TitlesFile, "titles", os.Getenv("MC_PS3_TITLES"), "NP_MATCHING_TITLES JSON (or a file holding the 256-hex passphrase); never printed")
	fs.StringVar(&c.Comm, "comm", env("MC_PS3_COMM", "NPWR05636_00"), "Minecraft PS3 communication id")
	fs.StringVar(&c.Room, "room", "", "room id, 12 hex digits (default: the host's room)")
	fs.StringVar(&c.PublicIP, "public-ip", os.Getenv("MC_PS3_PUBLIC_IP"), "our public IPv4 (the join publishes it; the STUN candidate falls back to it)")
	fs.StringVar(&c.LocalIP, "local-ip", os.Getenv("MC_PS3_LOCAL_IP"), "our LAN IPv4, offered as a local candidate")
	fs.IntVar(&c.Port, "port", 3658, "UDP port (the PS3 side's own is 3658)")
	fs.StringVar(&c.STUN, "stun", env("MC_PS3_STUN", "off"), "STUN server, or off")
	fs.DurationVar(&c.WaitOffer, "wait-offer", 15*time.Second, "give the host this long to offer first")
	fs.DurationVar(&c.Hold, "hold", 5*time.Minute, "how long to stay in the world")
	fs.IntVar(&c.Pad, "pad", 0, "our pad index (RUDP port = pad + 1)")
	fs.BoolVar(&c.PreLogin, "prelogin", true, "send E8 PreLogin after the hello")
	fs.DurationVar(&c.Nudge59, "nudge59", 0, "experiment: this long after signalling is established, if the host has not added us, set 0x59 to 0 and back (0x3206) so the host's 0x1107 path adds us (0 = off)")
	fs.Parse(os.Args[2:])
	if c.HostID == "" || c.TicketFile == "" || c.TitlesFile == "" || c.PublicIP == "" {
		return fmt.Errorf("joinps3 needs -host, -ticket-file, -titles and -public-ip")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	_, err := ps3join.Run(ctx, c)
	return err
}
