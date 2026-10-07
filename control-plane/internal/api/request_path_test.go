package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type countingStore struct {
	*store.MemoryStore
	counts, touches atomic.Int32
}

func (c *countingStore) CountAPIKeys(ctx context.Context) (int64, error) {
	c.counts.Add(1)
	return c.MemoryStore.CountAPIKeys(ctx)
}

func (c *countingStore) TouchAPIKey(ctx context.Context, id string) error {
	c.touches.Add(1)
	return c.MemoryStore.TouchAPIKey(ctx, id)
}

// Authenticated requests cost neither a count(*) (authentication is always on,
// so there is nothing to count) nor an UPDATE each.
func TestAuthMiddlewareNeverCountsKeysAndThrottlesTouches(t *testing.T) {
	cs := &countingStore{MemoryStore: newTestStore(t)}
	const secret = "key-for-request-path"
	if _, err := BootstrapAPIKey(context.Background(), cs, secret); err != nil {
		t.Fatal(err)
	}
	h := AuthMiddleware(cs, AuthConfig{})(testMux(NewServer(cs)))
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if n := cs.counts.Load(); n != 0 {
		t.Fatalf("100 requests counted the keys %d times, want none", n)
	}
	if n := cs.touches.Load(); n != 1 {
		t.Fatalf("100 requests touched the key %d times, want 1 per %s", n, keyTouchEvery)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("a request with no credential must be refused: got %d", rr.Code)
	}
}

func TestRequestLogKeepsFlushAndQuietsNodePolls(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	h := RequestLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("RequestLog hides http.Flusher from streaming handlers")
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("abc"))
	}))
	for _, target := range []string{"/v1/sandboxes", "/v1/nodes/n1/work", "/v1/nodes/n1/heartbeat", "/healthz"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	out := buf.String()
	for _, want := range []string{"path=/v1/sandboxes", "status=418", "bytes=3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Info log misses %q:\n%s", want, out)
		}
	}
	for _, quiet := range []string{"/v1/nodes/n1/work", "heartbeat", "/healthz"} {
		if strings.Contains(out, quiet) {
			t.Fatalf("%s must log at Debug, not Info:\n%s", quiet, out)
		}
	}
}
