package store

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// EnvSandboxIdleTimeout is the process env for the idle reaper.
	// Unset, 0, or off disables it (default), so short lab smokes do not flap.
	// Production/lab systemd should set ASP_SANDBOX_IDLE_TIMEOUT=2h (or 1h).
	EnvSandboxIdleTimeout = "ASP_SANDBOX_IDLE_TIMEOUT"
	// EnvSandboxIdleSweep is how often the control-plane loop calls StopIdleSandboxes.
	// Unset → 1m. Does not enable the reaper by itself.
	EnvSandboxIdleSweep = "ASP_SANDBOX_IDLE_SWEEP"

	// RecommendedIdleTimeout is the documented lab/production threshold (2h).
	// 1h is also a supported setting (ASP_SANDBOX_IDLE_TIMEOUT=1h). The process
	// default is disabled, not this value.
	RecommendedIdleTimeout = 2 * time.Hour

	// StopReasonIdle is persisted on sandboxes the reaper stops.
	StopReasonIdle = "idle_timeout"

	// IdleReapedMessage is the stable exec/API error. The CLI matches "idle timeout".
	IdleReapedMessage = "sandbox was stopped after idle timeout (reaped); start a new sandbox (asp session start --force)"
)

// IdleVerdict is the pure idle decision for one sandbox clock.
type IdleVerdict string

const (
	IdleDisabled IdleVerdict = "disabled"
	IdleActive   IdleVerdict = "active"
	IdleExpired  IdleVerdict = "expired"
)

// DecideIdle reports whether a sandbox should be reaped.
// timeout <= 0 is disabled (0 and "off"). lastActivity zero is treated as active
// so a missing clock does not mass-delete rows. now >= lastActivity+timeout is expired.
func DecideIdle(lastActivity, now time.Time, timeout time.Duration) IdleVerdict {
	if timeout <= 0 {
		return IdleDisabled
	}
	if lastActivity.IsZero() || now.IsZero() {
		return IdleActive
	}
	if !now.Before(lastActivity.Add(timeout)) {
		return IdleExpired
	}
	return IdleActive
}

// ParseIdleTimeout parses a duration flag/env value.
// Empty, 0, 0s, off, false, disabled, no, none → 0 (disabled).
// Otherwise Go duration syntax (2h, 1h, 90m, 120m).
func ParseIdleTimeout(raw string) (time.Duration, error) {
	v := strings.TrimSpace(strings.ToLower(raw))
	switch v {
	case "", "0", "0s", "off", "false", "disabled", "no", "none":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%w: idle timeout %q: %v", ErrInvalidInput, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%w: idle timeout must be >= 0", ErrInvalidInput)
	}
	return d, nil
}

// ResolveIdleTimeout picks flagValue when non-empty, otherwise envValue.
func ResolveIdleTimeout(envValue, flagValue string) (time.Duration, error) {
	raw := envValue
	if strings.TrimSpace(flagValue) != "" {
		raw = flagValue
	}
	return ParseIdleTimeout(raw)
}

// IdleTimeoutFromEnv reads ASP_SANDBOX_IDLE_TIMEOUT. Unset → disabled.
func IdleTimeoutFromEnv() (time.Duration, error) {
	return ParseIdleTimeout(os.Getenv(EnvSandboxIdleTimeout))
}

// IdleSweepInterval reads ASP_SANDBOX_IDLE_SWEEP. Invalid or empty → 1m.
// Values below 1s are clamped so a bad env cannot busy-loop the API.
func IdleSweepInterval(envValue string) time.Duration {
	v := strings.TrimSpace(envValue)
	if v == "" {
		return time.Minute
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Minute
	}
	if d < time.Second {
		return time.Second
	}
	return d
}
