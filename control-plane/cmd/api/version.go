package main

import (
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/version"
)

// wantsVersion reports whether the arguments ask which build this is. The control
// plane takes its settings from the environment and has no flag parser to hang
// --version on.
func wantsVersion(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "--version", "-version":
		return true
	}
	return false
}

// buildInfoCollector reports the build as the usual constant-1 gauge, so a dashboard
// can group by version and a rollout can be followed.
func buildInfoCollector() metrics.Collector {
	info := version.Get()
	return func() []metrics.Sample {
		return []metrics.Sample{{
			Name: "asp_build_info", Type: "gauge", Value: 1,
			Help: "The build of this control plane; always 1.",
			Labels: []metrics.Label{
				{Name: "version", Value: info.Short()},
				{Name: "commit", Value: info.Commit},
				{Name: "go_version", Value: info.GoVersion},
			},
		}}
	}
}
