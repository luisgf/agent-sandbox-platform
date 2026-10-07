package reconciler

import (
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/metrics"
)

// Metrics are what the reconciler counts: VM starts and how long they take, how a
// VM ended, disks the GC removed. A nil *Metrics counts nothing (tests, dry-run).
type Metrics struct {
	starts       *metrics.Counter   // asp_agent_vm_starts_total{kind,result}
	startSeconds *metrics.Histogram // asp_agent_vm_start_seconds{kind}
	guestReady   *metrics.Histogram // asp_agent_guest_ready_seconds{result}
	stops        *metrics.Counter   // asp_agent_vm_stops_total{how}
	exits        *metrics.Counter   // asp_agent_vm_exits_total{cause}
	gcRemoved    *metrics.Counter   // asp_agent_disk_gc_removed_total
	pollFailures *metrics.Counter   // asp_agent_poll_failures_total
}

// startBuckets: a VM boots in seconds; a slow disk copy or a guest that never
// answers takes minutes.
var startBuckets = []float64{.5, 1, 2, 3, 5, 8, 13, 21, 34, 55, 90, 150, 300}

// NewMetrics registers the reconciler's metrics on reg.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		starts: reg.Counter("asp_agent_vm_starts_total",
			"VM starts by kind (new or resume) and result: ok, setup (before the VMM: claim, network, workspace, disk), vmm_start, guest_not_ready, vmm_exited or report.",
			"kind", "result"),
		startSeconds: reg.Histogram("asp_agent_vm_start_seconds",
			"Time from claiming a sandbox to reporting it running, successful starts only.", startBuckets, "kind"),
		guestReady: reg.Histogram("asp_agent_guest_ready_seconds",
			"Time a start waited for the guest's pod-daemon to answer, by result (ready or timeout).", startBuckets, "result"),
		stops: reg.Counter("asp_agent_vm_stops_total",
			"VMs released by this node, by how: stop, delete, fence (the control plane no longer assigns it), exited (the VMM's process ended), start_failed or panic.", "how"),
		exits: reg.Counter("asp_agent_vm_exits_total",
			"VMM processes that ended without a stop asking them to, by cause: crash (killed or failed) or poweroff (the guest powered itself off).", "cause"),
		gcRemoved:    reg.Counter("asp_agent_disk_gc_removed_total", "Disks removed because no sandbox owns them."),
		pollFailures: reg.Counter("asp_agent_poll_failures_total", "Polls of the control plane that failed."),
	}
}

// recordStart counts one start that went as far as the VMM path.
func (m *Metrics) recordStart(kind string, began time.Time, err error) {
	if m == nil {
		return
	}
	result := startResult(err)
	m.starts.Inc(kind, result)
	if err == nil {
		m.startSeconds.Observe(time.Since(began).Seconds(), kind)
	}
}

// startResult names why a start ended as it did, from the error it returned.
func startResult(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, exitedPrefix):
		return "vmm_exited"
	case strings.Contains(msg, "guest_not_ready"):
		return "guest_not_ready"
	case strings.HasPrefix(msg, "vmm start:"):
		return "vmm_start"
	case strings.HasPrefix(msg, "report running:"):
		return "report"
	}
	return "setup"
}

func (m *Metrics) recordGuestWait(began time.Time, ready bool) {
	if m == nil {
		return
	}
	result := "timeout"
	if ready {
		result = "ready"
	}
	m.guestReady.Observe(time.Since(began).Seconds(), result)
}

func (m *Metrics) stop(how string) {
	if m == nil {
		return
	}
	if how == "" {
		how = "other"
	}
	m.stops.Inc(how)
}

func (m *Metrics) exit(cause string) {
	if m == nil {
		return
	}
	m.exits.Inc(cause)
}

func (m *Metrics) diskRemoved() {
	if m == nil {
		return
	}
	m.gcRemoved.Inc()
}

func (m *Metrics) pollFailed() {
	if m == nil {
		return
	}
	m.pollFailures.Inc()
}

// Collector reports, at scrape time, the VMs this reconciler holds.
func (r *Reconciler) Collector() metrics.Collector {
	return func() []metrics.Sample {
		r.mu.Lock()
		n := len(r.handles)
		r.mu.Unlock()
		return []metrics.Sample{{
			Name: "asp_agent_vms", Type: "gauge", Help: "VMs this node runs or is starting.", Value: float64(n),
		}}
	}
}
