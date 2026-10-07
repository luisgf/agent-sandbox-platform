package main

import (
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
)

func TestBufferedExecTimeoutFromEnv(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"":      api.DefaultBufferedExecTimeout,
		"  ":    api.DefaultBufferedExecTimeout,
		"45s":   45 * time.Second,
		"30m":   30 * time.Minute,
		"0":     0,
		"off":   0,
		" OFF ": 0,
		"none":  0,
	} {
		got, err := bufferedExecTimeoutFromEnv(in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-5s", "10", "1x"} {
		if _, err := bufferedExecTimeoutFromEnv(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if api.DefaultBufferedExecTimeout != 10*time.Minute {
		t.Fatalf("the default is %v", api.DefaultBufferedExecTimeout)
	}
}
