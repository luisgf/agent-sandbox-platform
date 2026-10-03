package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// RunIdleReaper periodically stops sandboxes with no activity for timeout.
// timeout <= 0 returns immediately (reaper disabled). Cancel ctx to stop the loop.
func (s *Server) RunIdleReaper(ctx context.Context, timeout, every time.Duration) {
	if timeout <= 0 || s == nil || s.Store == nil {
		return
	}
	if every <= 0 {
		every = time.Minute
	}
	s.sweepIdle(timeout)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepIdle(timeout)
		}
	}
}

func (s *Server) sweepIdle(timeout time.Duration) {
	stopped, err := s.Store.StopIdleSandboxes(time.Now().UTC(), timeout)
	if err != nil {
		slog.Error("sandbox idle reaper", "error", err)
		return
	}
	for _, sb := range stopped {
		slog.Info("sandbox idle-stopped", "id", sb.ID, "state", sb.State, "stop_reason", sb.StopReason, "idle_timeout", timeout.String())
	}
}

// idleExecBlock is non-empty when exec must not be proxied.
func idleExecBlock(sb store.Sandbox) string {
	if sb.StopReason == store.StopReasonIdle {
		return store.IdleReapedMessage
	}
	switch sb.State {
	case store.SandboxStopped, store.SandboxStopping, store.SandboxFailed:
		return "sandbox is " + string(sb.State)
	default:
		return ""
	}
}
