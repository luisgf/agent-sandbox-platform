package egress

import (
	"io"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
)

// Metrics are what the proxy and the DNS sink count. A nil *Metrics counts nothing.
type Metrics struct {
	requests *metrics.Counter // asp_agent_egress_requests_total{decision,reason,tenant}
	bytes    *metrics.Counter // asp_agent_egress_bytes_total{direction}
	dns      *metrics.Counter // asp_agent_egress_dns_queries_total{decision,tenant}
}

// NewMetrics registers the egress metrics on r.
func NewMetrics(r *metrics.Registry) *Metrics {
	return &Metrics{
		requests: r.Counter("asp_agent_egress_requests_total",
			"Requests the egress proxy decided on, by decision (allow or deny), reason (the kind of request that was allowed, or why it was denied) and the sandbox's tenant (none for a source that is not a sandbox).",
			"decision", "reason", "tenant"),
		bytes: r.Counter("asp_agent_egress_bytes_total",
			"Bytes the egress proxy carried: to_upstream from a guest, from_upstream back to it.", "direction"),
		dns: r.Counter("asp_agent_egress_dns_queries_total",
			"Queries the DNS sink answered, by decision: allow, deny (NXDOMAIN), error or unsupported.", "decision", "tenant"),
	}
}

func (m *Metrics) request(decision, reason, tenant string) {
	if m == nil {
		return
	}
	m.requests.Inc(decision, reason, tenant)
}

func (m *Metrics) add(direction string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.bytes.Add(float64(n), direction)
}

func (m *Metrics) query(decision, tenant string) {
	if m == nil {
		return
	}
	m.dns.Inc(decision, tenant)
}

// countingReader adds what it reads to a byte counter as it goes, so a long
// tunnel shows up in the metrics while it is open, not when it closes.
type countingReader struct {
	r         io.Reader
	m         *Metrics
	direction string
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.m.add(c.direction, int64(n))
	return n, err
}

// tenantLabel is the tenant to count a source under: its sandbox's, or "none".
func tenantLabel(c *PolicyCache, sandboxID string) string {
	if t := c.TenantOf(sandboxID); t != "" {
		return t
	}
	return "none"
}
