// Package wait polls a sandbox until it reaches a desired state.
package wait

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

// Getter fetches the current sandbox (injected for tests).
type Getter func(ctx context.Context, id string) (client.Sandbox, error)

// Options control WaitForState.
type Options struct {
	Timeout    time.Duration
	Initial    time.Duration
	MaxBackoff time.Duration
	Log        io.Writer // status transitions (stderr); may be nil
}

// WaitForState polls until sb.State == want or timeout / terminal failure.
func WaitForState(ctx context.Context, get Getter, id, want string, opt Options) (client.Sandbox, error) {
	if opt.Timeout <= 0 {
		opt.Timeout = 60 * time.Second
	}
	if opt.Initial <= 0 {
		opt.Initial = 200 * time.Millisecond
	}
	if opt.MaxBackoff <= 0 {
		opt.MaxBackoff = 2 * time.Second
	}
	deadline := time.Now().Add(opt.Timeout)
	backoff := opt.Initial
	var last string
	var sb client.Sandbox
	for {
		if err := ctx.Err(); err != nil {
			return sb, err
		}
		var err error
		sb, err = get(ctx, id)
		if err != nil {
			return sb, err
		}
		if sb.State != last {
			if opt.Log != nil {
				_, _ = fmt.Fprintf(opt.Log, "asp: sandbox %s state=%s\n", id, sb.State)
			}
			last = sb.State
		}
		if sb.State == want {
			return sb, nil
		}
		if isTerminalFailure(sb.State, want) {
			return sb, fmt.Errorf("sandbox %s reached terminal state %q while waiting for %q", id, sb.State, want)
		}
		if time.Now().After(deadline) {
			return sb, fmt.Errorf("timeout waiting for sandbox %s to become %q (last state=%q)", id, want, sb.State)
		}
		sleep := backoff
		remain := time.Until(deadline)
		if sleep > remain {
			sleep = remain
		}
		if sleep < 0 {
			sleep = 0
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()
			return sb, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > opt.MaxBackoff {
			backoff = opt.MaxBackoff
		}
	}
}

func isTerminalFailure(state, want string) bool {
	switch state {
	case "failed", "stopped":
		return state != want
	default:
		return false
	}
}
