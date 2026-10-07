package api

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// RetentionConfig bounds how long and how many stopped sandboxes keep their
// disks (ADR-0012).
type RetentionConfig struct {
	// TTL deletes a sandbox stopped for this long. 0 keeps them until deleted.
	TTL time.Duration
	// MaxStoppedPerTenant deletes a tenant's oldest stopped sandboxes beyond it. 0: no cap.
	MaxStoppedPerTenant int
	// Interval is how often the sweep runs.
	Interval time.Duration
}

// RetentionConfigFromEnv reads ASP_STOPPED_SANDBOX_TTL (default 7d),
// ASP_MAX_STOPPED_PER_TENANT (default no cap) and ASP_RETENTION_SWEEP (default 1m).
func RetentionConfigFromEnv() (RetentionConfig, error) {
	ttl, err := store.ParseRetentionTTL(os.Getenv(store.EnvStoppedSandboxTTL))
	if err != nil {
		return RetentionConfig{}, err
	}
	max, err := store.ParseMaxStoppedPerTenant(os.Getenv(store.EnvMaxStoppedPerTenant))
	if err != nil {
		return RetentionConfig{}, err
	}
	return RetentionConfig{TTL: ttl, MaxStoppedPerTenant: max, Interval: store.IdleSweepInterval(os.Getenv(store.EnvRetentionSweep))}, nil
}

// Enabled: something is bounded.
func (c RetentionConfig) Enabled() bool { return c.TTL > 0 || c.MaxStoppedPerTenant > 0 }

// RunRetentionReaper deletes stopped sandboxes past the TTL or over the
// per-tenant cap until ctx ends. It returns at once when neither is set.
func (s *Server) RunRetentionReaper(ctx context.Context, cfg RetentionConfig) {
	if !cfg.Enabled() || s == nil || s.Store == nil {
		return
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	s.sweepRetention(ctx, time.Now().UTC(), cfg)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepRetention(ctx, time.Now().UTC(), cfg)
		}
	}
}

func (s *Server) sweepRetention(ctx context.Context, now time.Time, cfg RetentionConfig) {
	ctx, cancel := context.WithTimeout(ctx, BackgroundDBTimeout)
	defer cancel()
	if expired, err := s.Store.ExpireStoppedSandboxes(ctx, now, cfg.TTL); err != nil {
		slog.Error("retention: expiring stopped sandboxes", "error", err)
	} else {
		for _, sb := range expired {
			slog.Info("stopped sandbox deleted: retention TTL", "id", sb.ID, "tenant", sb.TenantID, "state", sb.State, "ttl", cfg.TTL.String())
		}
	}
	if evicted, err := s.Store.EvictStoppedOverCap(ctx, cfg.MaxStoppedPerTenant); err != nil {
		slog.Error("retention: evicting stopped sandboxes over the tenant cap", "error", err)
	} else {
		for _, sb := range evicted {
			// The user's disk goes to make room: say so where an operator will see it.
			slog.Warn("stopped sandbox deleted to keep its tenant under the cap", "id", sb.ID, "tenant", sb.TenantID,
				"owner_sub", sb.OwnerSub, "max_stopped_per_tenant", cfg.MaxStoppedPerTenant)
		}
	}
}
