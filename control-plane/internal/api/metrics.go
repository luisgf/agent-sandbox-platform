package api

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/metrics"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// requestBuckets cover a heartbeat (milliseconds) and a streamed exec (minutes).
var requestBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600}

// serverMetrics are what the control plane counts as it works. What it holds at
// a moment (sandboxes by state, nodes, the database pool) is read at scrape time
// by collectors instead.
type serverMetrics struct {
	requests   *metrics.Counter   // asp_http_requests_total{route,code}
	duration   *metrics.Histogram // asp_http_request_duration_seconds{route}
	inflight   *metrics.Gauge     // asp_http_requests_in_flight
	creates    *metrics.Counter   // asp_sandbox_creates_total{result}
	reaped     *metrics.Counter   // asp_sandboxes_reaped_total{reason}
	lost       *metrics.Counter   // asp_sandboxes_lost_total{reason}
	nodeEvents *metrics.Counter   // asp_node_events_total{event}
	collectErr *metrics.Counter   // asp_collector_errors_total{collector}
}

func newServerMetrics(r *metrics.Registry) *serverMetrics {
	return &serverMetrics{
		requests:   r.Counter("asp_http_requests_total", "HTTP requests served, by route pattern and status code.", "route", "code"),
		duration:   r.Histogram("asp_http_request_duration_seconds", "Time to serve an HTTP request, by route pattern. A streamed exec lasts as long as its command.", requestBuckets, "route"),
		inflight:   r.Gauge("asp_http_requests_in_flight", "HTTP requests being served now."),
		creates:    r.Counter("asp_sandbox_creates_total", "Sandbox creations by outcome: ok, no_capacity (503), node_unavailable (409), invalid or error.", "result"),
		reaped:     r.Counter("asp_sandboxes_reaped_total", "Sandboxes the control plane stopped or deleted on its own, by reason: idle_timeout, retention_expired or tenant_cap.", "reason"),
		lost:       r.Counter("asp_sandboxes_lost_total", "Sandboxes failed because their node was lost.", "reason"),
		nodeEvents: r.Counter("asp_node_events_total", "Node liveness events: offline, fenced or fence_failed.", "event"),
		collectErr: r.Counter("asp_collector_errors_total", "Scrape-time reads that failed.", "collector"),
	}
}

// mx returns the server's metrics, creating the registry the first time. A Server
// made without NewServer works too.
func (s *Server) mx() *serverMetrics {
	s.metricsOnce.Do(func() {
		if s.Metrics == nil {
			s.Metrics = metrics.NewRegistry()
		}
		s.m = newServerMetrics(s.Metrics)
		s.Metrics.AddCollector(metrics.RuntimeCollector(time.Now()))
		s.Metrics.AddCollector(s.sandboxCollector)
		s.Metrics.AddCollector(s.nodeCollector)
	})
	return s.m
}

// MetricsRegistry is the registry /metrics serves, for adding collectors (the
// database pool).
func (s *Server) MetricsRegistry() *metrics.Registry {
	s.mx()
	return s.Metrics
}

// metricsInit is the embedded state; Server carries it.
type metricsInit struct {
	metricsOnce sync.Once
	m           *serverMetrics
}

// scrapeTimeout bounds what a collector reads from the store.
const scrapeTimeout = 10 * time.Second

func (s *Server) sandboxCollector() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	counts, err := s.Store.CountSandboxes(ctx)
	if err != nil {
		s.mx().collectErr.Inc("sandboxes")
		slog.Warn("metrics: counting sandboxes", "error", err)
		return nil
	}
	out := make([]metrics.Sample, 0, len(counts))
	for _, c := range counts {
		out = append(out, metrics.Sample{
			Name: "asp_sandboxes", Type: "gauge", Help: "Sandboxes by tenant and state, deleted ones included.",
			Labels: []metrics.Label{{Name: "tenant", Value: c.TenantID}, {Name: "state", Value: string(c.State)}},
			Value:  float64(c.Count),
		})
	}
	return out
}

func (s *Server) nodeCollector() []metrics.Sample {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	nodes, err := s.Store.ListNodes(ctx)
	if err != nil {
		s.mx().collectErr.Inc("nodes")
		slog.Warn("metrics: listing nodes", "error", err)
		return nil
	}
	usage, err := s.Store.ListNodeUsage(ctx)
	if err != nil {
		s.mx().collectErr.Inc("nodes")
		slog.Warn("metrics: node usage", "error", err)
		return nil
	}
	stopped, err := s.Store.CountStoppedByNode(ctx)
	if err != nil {
		s.mx().collectErr.Inc("nodes")
		slog.Warn("metrics: stopped sandboxes by node", "error", err)
		return nil
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	now := time.Now().UTC()
	var out []metrics.Sample
	add := func(name, help string, n store.Node, v float64) {
		out = append(out, metrics.Sample{Name: name, Help: help, Type: "gauge",
			Labels: []metrics.Label{{Name: "node", Value: n.ID}}, Value: v})
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	for _, n := range nodes {
		c := store.Candidate(n, usage[n.ID])
		reason := sched.Unschedulable(s.Sched, c, now)
		alive := n.RevokedAt == nil && n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) <= s.Sched.StaleAfter
		add("asp_node_up", "1 when the node has shown signs of life within the stale window and is not revoked.", n, b2f(alive))
		add("asp_node_schedulable", "1 when the scheduler would place a sandbox on the node.", n, b2f(reason == ""))
		add("asp_node_cordoned", "1 when an admin stopped new placements on the node.", n, b2f(n.Cordoned))
		add("asp_node_egress_enforced", "1 when the node forces its guests through its egress proxy.", n, b2f(n.EgressEnforced))
		add("asp_node_sandboxes", "Sandboxes placed on the node that hold its capacity.", n, float64(usage[n.ID].Sandboxes))
		add("asp_node_stopped_sandboxes", "Stopped sandboxes whose disks the node keeps.", n, float64(stopped[n.ID]))
		add("asp_node_allocated_cpu_millis", "CPU (milli-cores) allocated to sandboxes on the node.", n, float64(usage[n.ID].CPUMillis))
		add("asp_node_allocated_memory_mib", "Memory (MiB) allocated to sandboxes on the node.", n, float64(usage[n.ID].MemoryMiB))
		if n.LastSeenAt != nil {
			add("asp_node_last_seen_age_seconds", "Seconds since the node last showed a sign of life.", n, now.Sub(*n.LastSeenAt).Seconds())
		}
		if n.AgentVersion != "" {
			out = append(out, metrics.Sample{Name: "asp_node_agent_info", Type: "gauge", Value: 1,
				Help:   "1 per node, labelled with the version of its node-agent.",
				Labels: []metrics.Label{{Name: "node", Value: n.ID}, {Name: "version", Value: n.AgentVersion}}})
		}
		if n.DiskFreeMiB != nil {
			add("asp_node_disk_free_bytes", "Free space of the node's disk directory, as it last reported it.", n, float64(*n.DiskFreeMiB)*1024*1024)
		}
	}
	return out
}

// Instrument counts and times the requests of next, by the route pattern the mux
// matched (never the raw path, so a client cannot make up labels). It goes
// directly around the mux, inside the middleware that may copy the request: the
// mux fills in Pattern on the request it is given.
func (s *Server) Instrument(next http.Handler) http.Handler {
	m := s.mx()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		m.inflight.Add(1)
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			m.inflight.Add(-1)
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			m.requests.Inc(route, strconv.Itoa(status))
			m.duration.Observe(time.Since(started).Seconds(), route)
		}()
		next.ServeHTTP(rec, r)
	})
}

// authorizeMetrics lets through the callers that may see the node inventory: the
// numbers span every tenant. An IdP admin or operator, or a platform-scoped key;
// the open lab (no keys, no IdP) too, as for GET /v1/nodes.
func authorizeMetrics(w http.ResponseWriter, r *http.Request) bool {
	ctx := r.Context()
	if p, ok := IdPPrincipalFromContext(ctx); ok {
		if canViewNodes(p) {
			return true
		}
		forbid(w, "admin or operator role required to read the metrics")
		return false
	}
	if key, ok := APIKeyFromContext(ctx); ok && key.Scope != store.APIKeyScopePlatform {
		forbid(w, "a platform-scoped api key is required to read the metrics: they span every tenant")
		return false
	}
	return true
}

// Metrics serves GET /metrics in the Prometheus text format.
func (s *Server) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	if !authorizeMetrics(w, r) {
		return
	}
	s.mx()
	s.Metrics.Handler().ServeHTTP(w, r)
}
