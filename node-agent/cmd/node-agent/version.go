package main

import (
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/version"
)

// buildInfoCollector reports the build as the usual constant-1 gauge, so a dashboard
// can group by version and a rollout can be followed.
func buildInfoCollector() metrics.Collector {
	info := version.Get()
	return func() []metrics.Sample {
		return []metrics.Sample{{
			Name: "asp_agent_build_info", Type: "gauge", Value: 1,
			Help: "The build of this node-agent; always 1.",
			Labels: []metrics.Label{
				{Name: "version", Value: info.Short()},
				{Name: "commit", Value: info.Commit},
				{Name: "go_version", Value: info.GoVersion},
			},
		}}
	}
}
