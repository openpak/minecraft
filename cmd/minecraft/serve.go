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

	"github.com/openpak/minecraft/internal/httplog"
	"github.com/openpak/minecraft/internal/javaserver"
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
	rl := &relay.Relay{Conn: conn, Public: public, Store: st, JavaAddr: env("MINECRAFT_JAVA_ADDR", fmt.Sprintf("127.0.0.1:%d", javaPort)),
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
