// Package presence reports each console player who is in the shared world to the account core's live-session registry
// (namespace "minecraft", title "world"), so every network that shows friends can show them as online and joinable. It is the
// same registry the console adapters publish to; nothing here is Minecraft-specific storage (card openpak/prds#162).
package presence

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Namespace and Title are what the core holds for a player in the world; adapters render a friend with this session as being in
// the shared Minecraft world.
const (
	Namespace = "minecraft"
	Title     = "world"
)

const (
	lease     = 45 * time.Second
	heartbeat = 15 * time.Second
)

// Reporter publishes presence. A zero Reporter (or a nil one) does nothing, so the service runs without a core.
type Reporter struct {
	Sessions   accountv1.SessionsClient
	ResolveURL string // nn-account's /internal/resolve/pid base, for Wii U PIDs
	ResolveKey string
	HTTP       *http.Client
}

// Dial connects to the account core's gRPC (addr host:port) with the internal key.
func Dial(addr, coreKey, resolveURL, resolveKey string) (*Reporter, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, o ...grpc.CallOption) error {
			return inv(metadata.AppendToOutgoingContext(ctx, "x-internal-key", coreKey), method, req, reply, cc, o...)
		}))
	if err != nil {
		return nil, err
	}
	return &Reporter{Sessions: accountv1.NewSessionsClient(conn), ResolveURL: resolveURL, ResolveKey: resolveKey}, nil
}

func (r *Reporter) account(ctx context.Context, pid uint32) (string, error) {
	c := r.HTTP
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Second}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/internal/resolve/pid?namespace=wiiu&pid=%s", r.ResolveURL, url.QueryEscape(fmt.Sprint(pid))), nil)
	req.Header.Set("X-Internal-Key", r.ResolveKey)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Found   bool   `json:"found"`
		Account string `json:"account_id"`
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("resolve answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if !out.Found {
		return "", fmt.Errorf("no account behind Wii U PID %d", pid)
	}
	return out.Account, nil
}

// Join starts publishing a Wii U player (by NEX PID) as being in the world and returns the function that ends it. The lease is
// renewed in the background, so a service that dies takes the presence with it within the lease.
func (r *Reporter) Join(pid uint32) (leave func()) {
	if r == nil || r.Sessions == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		acct, sid := "", ""
		register := func() {
			if acct == "" {
				a, err := r.account(ctx, pid)
				if err != nil {
					slog.Warn("presence: no account", "pid", pid, "err", err)
					return
				}
				acct = a
			}
			resp, err := r.Sessions.RegisterSession(ctx, &accountv1.RegisterSessionRequest{AccountId: acct, Namespace: Namespace, TitleId: Title,
				EndpointRef: fmt.Sprintf("minecraft:wiiu:%d", pid), LeaseSeconds: int32(lease / time.Second), Client: "wiiu"})
			if err != nil {
				slog.Warn("presence: register", "pid", pid, "err", err)
				return
			}
			sid = resp.GetSessionId()
			slog.Info("presence: in the world", "pid", pid)
		}
		register()
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				if sid != "" {
					ec, c := context.WithTimeout(context.Background(), 5*time.Second)
					_, err := r.Sessions.ExpireSession(ec, &accountv1.ExpireSessionRequest{SessionId: sid})
					c()
					slog.Info("presence: left the world", "pid", pid, "err", err)
				}
				return
			case <-t.C:
				if sid == "" {
					register()
				} else if _, err := r.Sessions.Heartbeat(ctx, &accountv1.HeartbeatRequest{SessionId: sid}); err != nil {
					sid = ""
					register()
				}
			}
		}
	}()
	return func() { cancel(); wg.Wait() }
}
