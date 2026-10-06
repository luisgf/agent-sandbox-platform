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

// execBlock is non-empty when exec must not be proxied: only a running
// sandbox has a guest the node agent can reach. The caller answers 409.
func execBlock(sb store.Sandbox) string {
	switch sb.StopReason {
	case store.StopReasonIdle:
		return store.IdleReapedMessage
	case store.StopReasonNodeLost, store.StopReasonAgentRestarted:
		return store.NodeLostMessage
	}
	switch sb.State {
	case store.SandboxRunning:
		return ""
	case store.SandboxRequested, store.SandboxScheduled, store.SandboxStarting:
		return "sandbox is " + string(sb.State) + "; wait until it is running"
	default:
		return "sandbox is " + string(sb.State)
	}
}
