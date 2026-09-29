package wait

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func TestWaitForStateSuccess(t *testing.T) {
	states := []string{"requested", "starting", "running"}
	i := 0
	get := func(ctx context.Context, id string) (client.Sandbox, error) {
		s := states[i]
		if i < len(states)-1 {
			i++
		}
		return client.Sandbox{ID: id, State: s}, nil
	}
	var log strings.Builder
	sb, err := WaitForState(context.Background(), get, "sb1", "running", Options{
		Timeout:    2 * time.Second,
		Initial:    5 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond,
		Log:        &log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != "running" {
		t.Fatalf("state=%s", sb.State)
	}
	if !strings.Contains(log.String(), "requested") || !strings.Contains(log.String(), "running") {
		t.Fatalf("log=%q", log.String())
	}
}

func TestWaitForStateFailed(t *testing.T) {
	get := func(ctx context.Context, id string) (client.Sandbox, error) {
		return client.Sandbox{ID: id, State: "failed"}, nil
	}
	_, err := WaitForState(context.Background(), get, "sb1", "running", Options{
		Timeout: time.Second,
		Log:     io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err=%v", err)
	}
}

func TestWaitForStateTimeout(t *testing.T) {
	get := func(ctx context.Context, id string) (client.Sandbox, error) {
		return client.Sandbox{ID: id, State: "starting"}, nil
	}
	_, err := WaitForState(context.Background(), get, "sb1", "running", Options{
		Timeout:    40 * time.Millisecond,
		Initial:    5 * time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
		Log:        io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err=%v", err)
	}
}
