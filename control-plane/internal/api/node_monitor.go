package api

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// The node monitor (ADR-0011) watches node liveness: a node silent for longer
// than the stale window is marked offline (the scheduler already skips it); after
// the failover delay its sandboxes are failed, since their disks live on that
// server. A revoked node is lost at once. Silence is measured from the later of
// the node's last sign of life and the monitor's start, so restarting the
// control plane after downtime does not fail every sandbox at once.

const (
	EnvNodeMonitorInterval = "ASP_NODE_MONITOR_INTERVAL"
	EnvNodeFailoverAfter   = "ASP_NODE_FAILOVER_AFTER"
)

// NodeMonitorConfig: Interval between sweeps; StaleAfter before a silent node is
// marked offline; FailoverAfter before its sandboxes are failed (0 disables that,
// except for revoked nodes).
type NodeMonitorConfig struct {
	Interval      time.Duration
	StaleAfter    time.Duration
	FailoverAfter time.Duration
}

// NodeMonitorConfigFromEnv reads ASP_NODE_MONITOR_INTERVAL (15s) and
// ASP_NODE_FAILOVER_AFTER (5m; 0 or off disables failover). staleAfter comes
// from the scheduler (ASP_NODE_STALE_AFTER).
func NodeMonitorConfigFromEnv(staleAfter time.Duration) (NodeMonitorConfig, error) {
	cfg := NodeMonitorConfig{Interval: 15 * time.Second, StaleAfter: staleAfter, FailoverAfter: 5 * time.Minute}
	if v := strings.TrimSpace(os.Getenv(EnvNodeMonitorInterval)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return NodeMonitorConfig{}, fmt.Errorf("%s=%q: want a duration greater than 0", EnvNodeMonitorInterval, v)
		}
		cfg.Interval = d
	}
	if v := strings.TrimSpace(os.Getenv(EnvNodeFailoverAfter)); v != "" {
		if v == "0" || strings.EqualFold(v, "off") {
			cfg.FailoverAfter = 0
		} else {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return NodeMonitorConfig{}, fmt.Errorf("%s=%q: want a duration, 0 or off", EnvNodeFailoverAfter, v)
			}
			cfg.FailoverAfter = d
		}
	}
	if cfg.StaleAfter <= cfg.Interval {
		return NodeMonitorConfig{}, fmt.Errorf("node stale window (%s) must be longer than %s (%s)", cfg.StaleAfter, EnvNodeMonitorInterval, cfg.Interval)
	}
	if cfg.FailoverAfter > 0 && cfg.FailoverAfter < cfg.StaleAfter {
		return NodeMonitorConfig{}, fmt.Errorf("%s (%s) must not be shorter than the stale window (%s)", EnvNodeFailoverAfter, cfg.FailoverAfter, cfg.StaleAfter)
	}
	return cfg, nil
}

// RunNodeMonitor sweeps nodes every cfg.Interval until ctx is cancelled.
func (s *Server) RunNodeMonitor(ctx context.Context, cfg NodeMonitorConfig) {
	if s == nil || s.Store == nil || cfg.Interval <= 0 {
		return
	}
	started := time.Now().UTC()
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sweepNodes(ctx, now.UTC(), started, cfg)
		}
	}
}

func (s *Server) sweepNodes(ctx context.Context, now, started time.Time, cfg NodeMonitorConfig) {
	nodes, err := s.Store.ListNodes()
	if err != nil {
		slog.Error("node monitor: list nodes", "error", err)
		return
	}
	var usage map[string]store.NodeUsage // read once, only if a node is lost
	for _, n := range nodes {
		ep := store.EffectiveAgentEndpoint(n)
		if ep == "" || strings.HasPrefix(ep, "local://") {
			continue // stub rows run nothing
		}
		lastSign := started
		if n.LastSeenAt != nil && n.LastSeenAt.After(started) {
			lastSign = *n.LastSeenAt
		}
		silent := now.Sub(lastSign)

		if n.RevokedAt == nil && n.State != "offline" && silent > cfg.StaleAfter {
			changed, err := s.Store.MarkNodeOffline(n.ID, now.Add(-cfg.StaleAfter))
			if err != nil {
				slog.Error("node monitor: mark offline", "node_id", n.ID, "error", err)
			} else if changed {
				slog.Warn("node offline: no signs of life", "node_id", n.ID, "silent_for", silent.Round(time.Second).String())
			}
		}

		lost := n.RevokedAt != nil || (cfg.FailoverAfter > 0 && silent > cfg.FailoverAfter)
		if !lost {
			continue
		}
		silentSince := now.Add(-cfg.FailoverAfter)
		if n.RevokedAt != nil {
			silentSince = now
		}
		if usage == nil {
			if usage, err = s.Store.ListNodeUsage(); err != nil {
				slog.Error("node monitor: usage", "error", err)
				usage = map[string]store.NodeUsage{}
			}
		}
		if usage[n.ID].Sandboxes > 0 {
			s.fenceLostNode(ctx, n, lastSign)
		}
		failed, err := s.Store.FailNodeSandboxes(n.ID, store.StopReasonNodeLost, silentSince)
		if err != nil {
			slog.Error("node monitor: fail sandboxes of a lost node", "node_id", n.ID, "error", err)
			continue
		}
		for _, sb := range failed {
			slog.Warn("sandbox lost with its node", "sandbox_id", sb.ID, "node_id", n.ID, "state", sb.State,
				"revoked", n.RevokedAt != nil, "silent_for", silent.Round(time.Second).String())
		}
	}

	// Rows created before placement at create have no node and never run.
	if failed, err := s.Store.FailUnassignedRequested(now.Add(-cfg.StaleAfter), store.StopReasonUnscheduled); err != nil {
		slog.Error("node monitor: unassigned sandboxes", "error", err)
	} else {
		for _, sb := range failed {
			slog.Warn("sandbox without a node failed", "sandbox_id", sb.ID, "stop_reason", sb.StopReason)
		}
	}
}

// fenceLostNode fences a lost node once per outage (keyed by its last sign of
// life). A failed fence is logged and recorded; the sandboxes are failed anyway:
// nothing restarts them elsewhere, and the node stops them itself when it comes
// back and its lease renewals are refused.
func (s *Server) fenceLostNode(ctx context.Context, n store.Node, lastSign time.Time) {
	s.fenceMu.Lock()
	if s.fencedOutage == nil {
		s.fencedOutage = map[string]time.Time{}
	}
	if done, ok := s.fencedOutage[n.ID]; ok && done.Equal(lastSign) {
		s.fenceMu.Unlock()
		return
	}
	s.fencedOutage[n.ID] = lastSign
	s.fenceMu.Unlock()

	provider := "none"
	if s.Fence != nil {
		provider = s.Fence.Name()
	}
	fenced, err := s.fenceNode(ctx, n)
	switch {
	case err != nil:
		slog.Error("fence of a lost node failed; failing its sandboxes anyway", "node_id", n.ID, "provider", provider, "error", err)
		_ = s.Store.EmitNodeEvent(n.ID, "node.fence_failed", "node-monitor", map[string]any{"provider": provider, "error": err.Error()})
	case fenced:
		slog.Warn("fenced a lost node", "node_id", n.ID, "provider", provider)
		_ = s.Store.EmitNodeEvent(n.ID, "node.fenced", "node-monitor", map[string]any{"provider": provider})
	}
}
