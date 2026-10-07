package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
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
// sandbox has a guest the node agent can reach. The caller answers 409. The
// message names what to do: wait, resume (the disk of a stopped sandbox is
// kept), or start a new sandbox.
func execBlock(sb store.Sandbox) string {
	lost := sb.StopReason == store.StopReasonNodeLost || sb.StopReason == store.StopReasonAgentRestarted
	switch sb.State {
	case store.SandboxRunning:
		return ""
	case store.SandboxRequested, store.SandboxScheduled, store.SandboxStarting:
		return "sandbox is " + string(sb.State) + "; wait until it is running"
	case store.SandboxStopping, store.SandboxStopped:
		switch {
		case lost:
			return store.LostAdvice(sb.State, sb.StopReason, nodeIDOf(sb))
		case sb.StopReason == store.StopReasonIdle:
			return store.IdleReapedMessage
		case sb.State == store.SandboxStopped:
			msg := "sandbox is stopped; its disk is kept: resume it (asp session resume)"
			if sb.StatusDetail != "" {
				msg += " (the last resume failed: " + strings.TrimPrefix(sb.StatusDetail, "resume failed: ") + ")"
			}
			return msg
		}
		return "sandbox is stopping"
	case store.SandboxFailed:
		if lost {
			return store.LostAdvice(sb.State, sb.StopReason, nodeIDOf(sb))
		}
		if sb.StatusDetail != "" {
			return "sandbox failed: " + sb.StatusDetail
		}
		return "sandbox failed"
	case store.SandboxDeleting:
		return "sandbox is being deleted"
	case store.SandboxDeleted:
		return "sandbox was deleted"
	}
	return "sandbox is " + string(sb.State)
}

func nodeIDOf(sb store.Sandbox) string {
	if sb.NodeID == nil {
		return ""
	}
	return *sb.NodeID
}

// touchEvery is how often keepActive touches a sandbox for an idle timeout: a few
// times inside it, so one missed touch does not let the reaper in, and at most
// once a minute. 0 when the reaper is off.
func touchEvery(idleTimeout time.Duration) time.Duration {
	if idleTimeout <= 0 {
		return 0
	}
	every := idleTimeout / 3
	if every > time.Minute {
		every = time.Minute
	}
	if every < 10*time.Millisecond {
		every = 10 * time.Millisecond
	}
	return every
}

// keepActive marks the sandbox active now and again at touchEvery until the
// returned function is called. Idle means "no exec running and none finished for
// the idle timeout": the reaper must not stop a sandbox in the middle of a
// command that streams for longer than the timeout.
func (s *Server) keepActive(sandboxID string) (stop func()) {
	_ = s.Store.TouchSandboxActivity(sandboxID)
	every := touchEvery(s.IdleTimeout)
	if every == 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = s.Store.TouchSandboxActivity(sandboxID)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
