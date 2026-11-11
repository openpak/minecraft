package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/openpak/minecraft/internal/directory"
	"github.com/openpak/minecraft/internal/httplog"
	"github.com/openpak/minecraft/internal/javaserver"
	"github.com/openpak/minecraft/internal/presence"
	"github.com/openpak/minecraft/internal/relay"
	"github.com/openpak/minecraft/internal/store"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// serve runs the service: the Java world server, the Wii U leg on UDP (MINECRAFT_RELAY_ADDR) and the internal HTTP surface
// (MINECRAFT_HTTP_ADDR): /health, /healthz and /internal/legs for the nex Minecraft title (bearer MINECRAFT_INTERNAL_KEY).
func serve() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	public, err := netip.ParseAddrPort(env("MINECRAFT_RELAY_PUBLIC", "127.0.0.1:22960"))
	if err != nil {
		return fmt.Errorf("MINECRAFT_RELAY_PUBLIC: %w", err)
	}
	var st *store.Store
	if url := os.Getenv("MINECRAFT_DATABASE_URL"); url != "" {
		if st, err = store.Open(ctx, url); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer st.Close()
	} else {
		slog.Warn("MINECRAFT_DATABASE_URL unset: sessions are not recorded")
	}
	view, _ := strconv.Atoi(env("MINECRAFT_VIEW_DISTANCE", "6"))
	javaPort := 25565
	if os.Getenv("MINECRAFT_JAVA_EXTERNAL") == "" {
		if err := javaserver.Run(ctx, javaserver.Config{Dir: env("MINECRAFT_JAVA_DIR", "/data"), Java: env("MINECRAFT_JAVA_BIN", "java"),
			Xmx: env("MINECRAFT_JAVA_XMX", "768m"), Port: javaPort, ViewDistance: view, Motd: "OpenPak Minecraft", NativeEpoll: os.Getenv("MINECRAFT_JAVA_EPOLL") == "1"}); err != nil {
			return fmt.Errorf("java server: %w", err)
		}
	}
	conn, err := net.ListenPacket("udp4", env("MINECRAFT_RELAY_ADDR", ":22960"))
	if err != nil {
		return err
	}
	var pres *presence.Reporter
	if addr := os.Getenv("MINECRAFT_CORE_ADDRESS"); addr != "" {
		if pres, err = presence.Dial(addr, os.Getenv("MINECRAFT_CORE_INTERNAL_KEY"), env("MINECRAFT_RESOLVE_URL", "http://openpak-nn-account:20050"), os.Getenv("MINECRAFT_RESOLVE_KEY")); err != nil {
			return fmt.Errorf("account core: %w", err)
		}
	} else {
		slog.Warn("MINECRAFT_CORE_ADDRESS unset: players are not published to the account core")
	}
	rl := &relay.Relay{Presence: pres, Conn: conn, Public: public, Store: st, JavaAddr: env("MINECRAFT_JAVA_ADDR", fmt.Sprintf("127.0.0.1:%d", javaPort)),
		HostName: env("MINECRAFT_HOST_NAME", "OpenPak"), ViewDistance: view}
	go func() {
		if err := rl.Serve(ctx); err != nil && ctx.Err() == nil {
			slog.Error("relay stopped", "err", err)
			stop()
		}
	}()
	key := os.Getenv("MINECRAFT_INTERNAL_KEY")
	mux := http.NewServeMux()
	health := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) }
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /healthz", health) // the website's /healthz/minecraft and the status page read this one
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /internal/legs", authed(func(w http.ResponseWriter, r *http.Request) {
		var s relay.LegSpec
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&s); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id, err := rl.Register(r.Context(), s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "station": public.String()})
	}))
	mux.HandleFunc("POST /internal/probe", authed(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Gathering int64  `json:"gathering"`
			Address   string `json:"address"`
			Port      uint16 `json:"port"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&q); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ip, err := netip.ParseAddr(q.Address)
		if err != nil || q.Port == 0 {
			http.Error(w, "bad address", http.StatusBadRequest)
			return
		}
		if err := rl.ProbeJoiner(q.Gathering, netip.AddrPortFrom(ip, q.Port)); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Write([]byte("ok\n"))
	}))
	if dir := startDirectory(ctx, pres); dir != nil {
		mux.HandleFunc("GET /internal/directory", authed(func(w http.ResponseWriter, _ *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{"games": dir.Games(), "joins": dir.RecentJoins()})
		}))
	}
	mux.HandleFunc("GET /internal/legs", authed(func(w http.ResponseWriter, _ *http.Request) { json.NewEncoder(w).Encode(rl.Legs()) }))
	srv := &http.Server{Addr: env("MINECRAFT_HTTP_ADDR", ":20270"), Handler: httplog.Middleware(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(sh)
		conn.Close()
	}()
	slog.Info("minecraft up", "version", version, "http", srv.Addr, "relay", conn.LocalAddr().String(), "public", public.String())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// startDirectory runs the cross-play session directory (prds#177) when the Wii U and PS3 internal surfaces are configured: a game a
// player hosts on one console shows on the other's network under the host's name. MINECRAFT_NEX_URL and MINECRAFT_INTERNAL_KEY reach nex
// minecraft-wiiu; MINECRAFT_PS3_MATCHING_URL, MINECRAFT_PS3_COMMUNITY_URL and MINECRAFT_PS3_KEY reach np-matching2 and np-community
// (their NP_BRIDGE_KEY). Without them nothing runs, as before.
func startDirectory(ctx context.Context, pres *presence.Reporter) *directory.Directory {
	nexURL, matching, community := os.Getenv("MINECRAFT_NEX_URL"), os.Getenv("MINECRAFT_PS3_MATCHING_URL"), os.Getenv("MINECRAFT_PS3_COMMUNITY_URL")
	if nexURL == "" || matching == "" || community == "" {
		slog.Warn("session directory off: set MINECRAFT_NEX_URL, MINECRAFT_PS3_MATCHING_URL and MINECRAFT_PS3_COMMUNITY_URL")
		return nil
	}
	wiiu := &directory.WiiU{Nex: nexURL, NexKey: os.Getenv("MINECRAFT_INTERNAL_KEY"), ResolveURL: env("MINECRAFT_RESOLVE_URL", "http://openpak-nn-account:20050"),
		ResolveKey: os.Getenv("MINECRAFT_RESOLVE_KEY")}
	if pres != nil && pres.Sessions != nil {
		wiiu.Presence = directory.CorePresence{Sessions: pres.Sessions}
	}
	ps3 := &directory.PS3{Matching: matching, Community: community, Key: os.Getenv("MINECRAFT_PS3_KEY"), Title: env("MINECRAFT_PS3_TITLE", "NPWR05636_00")}
	d := &directory.Directory{Adapters: []directory.Adapter{wiiu, ps3}}
	go d.Run(ctx, 5*time.Second)
	slog.Info("session directory on", "platforms", "wiiu,ps3")
	return d
}

func healthcheck() error {
	c := http.Client{Timeout: 3 * time.Second}
	addr := env("MINECRAFT_HTTP_ADDR", ":20270")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	r, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("status %d", r.StatusCode)
	}
	return nil
}
