package presence

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
	"google.golang.org/grpc"
)

type fake struct {
	accountv1.SessionsClient
	mu     sync.Mutex
	reg    []*accountv1.RegisterSessionRequest
	expire int
}

func (f *fake) RegisterSession(_ context.Context, r *accountv1.RegisterSessionRequest, _ ...grpc.CallOption) (*accountv1.RegisterSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reg = append(f.reg, r)
	return &accountv1.RegisterSessionResponse{SessionId: "s1"}, nil
}
func (f *fake) ExpireSession(context.Context, *accountv1.ExpireSessionRequest, ...grpc.CallOption) (*accountv1.ExpireSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire++
	return &accountv1.ExpireSessionResponse{}, nil
}

// A player in the world is a "minecraft/world" core session for their account, ended when they leave.
func TestJoinLeave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Key") != "k" || r.URL.Query().Get("pid") != "1700000001" || r.URL.Query().Get("namespace") != "wiiu" {
			http.Error(w, "no", 403)
			return
		}
		w.Write([]byte(`{"found":true,"account_id":"acct-1"}`))
	}))
	defer srv.Close()
	f := &fake{}
	leave := (&Reporter{Sessions: f, ResolveURL: srv.URL, ResolveKey: "k"}).Join(1700000001)
	for i := 0; i < 100; i++ {
		f.mu.Lock()
		n := len(f.reg)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	leave()
	if len(f.reg) != 1 || f.reg[0].AccountId != "acct-1" || f.reg[0].Namespace != Namespace || f.reg[0].TitleId != Title || f.expire != 1 {
		t.Fatalf("reg=%v expire=%d", f.reg, f.expire)
	}
}

func TestNilReporter(t *testing.T) { (*Reporter)(nil).Join(1)() }
