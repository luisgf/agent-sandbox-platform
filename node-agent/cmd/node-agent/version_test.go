package main

import (
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/version"
)

func TestVersionFlag(t *testing.T) {
	cfg, err := parse(t, nil, "--version")
	if err != nil || !cfg.ShowVersion {
		t.Fatalf("--version: %+v %v", cfg.ShowVersion, err)
	}
	// Not a setting an environment can turn on.
	cfg, err = parse(t, map[string]string{"ASP_VERSION": "1"})
	if err != nil || cfg.ShowVersion {
		t.Fatalf("ASP_VERSION must not turn --version on: %v %v", cfg.ShowVersion, err)
	}
}

func TestRegisterRequestSaysWhichBuildIsRegistering(t *testing.T) {
	if got := registerRequest(config{NodeID: "n1"}).AgentVersion; got != version.Short() || got == "" {
		t.Fatalf("agent_version = %q, want %q", got, version.Short())
	}
}

func TestAgentBuildInfoIsExposed(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.AddCollector(buildInfoCollector())
	var sb strings.Builder
	if _, err := reg.WriteTo(&sb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "asp_agent_build_info{") || !strings.Contains(sb.String(), `version="`) {
		t.Fatalf("no build info in:\n%s", sb.String())
	}
}
