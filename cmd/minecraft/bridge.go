package main

import (
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openpak/minecraft/internal/bridge"
	"github.com/openpak/minecraft/internal/directory"
)

// newBridge builds the cross-play bridge of a Wii U console joining a game a real PS3 hosts (prds#177). Environment, all optional but the
// first two (without them nothing is bridged and the directory only advertises):
//
//	MINECRAFT_PS3_MATCHING_URL, MINECRAFT_PS3_KEY   np-matching2's internal listener and its NP_BRIDGE_KEY (the identity route)
//	MINECRAFT_PS3_TITLE                              communication id (NPWR05636_00)
//	MINECRAFT_PS3_M2_PLAIN  (127.0.0.1:3480)         np-matching2's plain Matching2 port
//	MINECRAFT_PS3_M2_TLS    (127.0.0.1:443)          its TLS front (the firmware's port 443; routed on the SNI matching.np.community.playstation.net)
//	MINECRAFT_PS3_RT        (127.0.0.1:5223)         np-community's RT, MINECRAFT_PS3_RT_TLS (1) whether it is TLS
//	MINECRAFT_PS3_BIND      (host of MINECRAFT_RELAY_ADDR)  local address of the PS3 leg's UDP socket: the box's second address, so replies leave from it
//	MINECRAFT_PS3_PUBLIC_IP (host of MINECRAFT_RELAY_PUBLIC) the address the home PS3 sends to (the signalling candidates carry it)
//	MINECRAFT_PS3_UDP_PORTS (3658-3667)              one port per PS3 leg, first free one is taken: open them to the PS3 (UDP)
//	MINECRAFT_PS3_WAIT_OFFER (10s)                   let the host offer first, then we offer
//	MINECRAFT_BRIDGE_TRACE_DIR (/data/bridge)        per-session trace files
//	MINECRAFT_BRIDGE_OFF=1                           no bridge
func newBridge(public netip.AddrPort, dir *directory.Directory, wiiu *directory.WiiU) *bridge.Manager {
	matching := os.Getenv("MINECRAFT_PS3_MATCHING_URL")
	key := os.Getenv("MINECRAFT_PS3_KEY")
	if matching == "" || key == "" {
		slog.Warn("bridge off: MINECRAFT_PS3_MATCHING_URL and MINECRAFT_PS3_KEY are not set")
		return nil
	}
	bind := os.Getenv("MINECRAFT_PS3_BIND")
	if bind == "" {
		if h, _, err := net.SplitHostPort(env("MINECRAFT_RELAY_ADDR", "")); err == nil && h != "" && h != "0.0.0.0" {
			bind = h
		}
	}
	pub := env("MINECRAFT_PS3_PUBLIC_IP", public.Addr().String())
	lo, hi := 3658, 3667
	if a, b, ok := strings.Cut(env("MINECRAFT_PS3_UDP_PORTS", "3658-3667"), "-"); ok {
		lo, _ = strconv.Atoi(a)
		hi, _ = strconv.Atoi(b)
	}
	wait, _ := time.ParseDuration(env("MINECRAFT_PS3_WAIT_OFFER", "10s"))
	m := &bridge.Manager{Config: bridge.Config{
		Matching: strings.TrimRight(matching, "/"), Key: key, Comm: env("MINECRAFT_PS3_TITLE", "NPWR05636_00"),
		M2Plain: env("MINECRAFT_PS3_M2_PLAIN", "127.0.0.1:3480"), M2TLS: env("MINECRAFT_PS3_M2_TLS", "127.0.0.1:443"),
		M2Name: "matching.np.community.playstation.net", RT: env("MINECRAFT_PS3_RT", "127.0.0.1:5223"), RTTLS: env("MINECRAFT_PS3_RT_TLS", "1") == "1",
		BindIP: bind, PublicIP: pub, PortLo: lo, PortHi: hi, WaitOffer: wait,
		TraceDir:    env("MINECRAFT_BRIDGE_TRACE_DIR", "/data/bridge"),
		ResolveWiiU: wiiu.ResolvePID, Game: dir.Lookup,
	}}
	slog.Info("bridge on: Wii U joiners of PS3-hosted games", "ps3_udp", pub+":"+strconv.Itoa(lo)+"-"+strconv.Itoa(hi), "bind", bind, "trace", m.TraceDir)
	return m
}
