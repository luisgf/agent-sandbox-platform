package main

import (
	"context"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/metrics"
)

func TestWantsVersion(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		{[]string{"version"}, true},
		{[]string{"-idle-timeout", "2h"}, false},
		{[]string{"-idle-timeout", "2h", "--version"}, false},
	} {
		if got := wantsVersion(c.args); got != c.want {
			t.Errorf("wantsVersion(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// --version prints and returns before anything is read from the environment or started.
func TestVersionStartsNothing(t *testing.T) {
	t.Setenv("ASP_LISTEN_ADDR", "not an address")
	if err := run(context.Background(), []string{"--version"}); err != nil {
		t.Fatalf("run --version: %v", err)
	}
}

func TestBuildInfoIsExposed(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.AddCollector(buildInfoCollector())
	var sb strings.Builder
	if _, err := reg.WriteTo(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "asp_build_info{") || !strings.Contains(sb.String(), `version="`) {
		t.Fatalf("no build info in:\n%s", sb.String())
	}
}
