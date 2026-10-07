package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

// A VM's process that ends while its sandbox runs (killed, out of memory, a
// fault in the VMM, a guest that powered itself off) leaves the control plane
// believing the sandbox still runs, with an exec that fails and capacity held for
// nothing. The VMM tells the reconciler (vmm.ExitNotifier); the next poll reports
// the sandbox stopped, with its disk and the cause, so it can be resumed.

// exitedPrefix starts the status detail of a sandbox whose VM ended on its own.
// The control plane (store.VMMExitedDetailPrefix) keeps such a detail with a stop
// reason clients can switch on.
const exitedPrefix = "vmm_exited:"

// exitDetailMax bounds the cause in the detail: it comes from a process's output.
const exitDetailMax = 300

// exitDetail is the status detail for a VM that ended on its own: how, and after
// how long.
func exitDetail(info vmm.ExitInfo) string {
	how := "the guest powered off"
	if info.Err != nil {
		how = strings.Join(strings.Fields(info.Err.Error()), " ")
		if len(how) > exitDetailMax {
			how = how[:exitDetailMax] + "..."
		}
	}
	lived := info.Lived.Round(time.Second)
	if info.Lived < 10*time.Second {
		lived = info.Lived.Round(100 * time.Millisecond)
	}
	return fmt.Sprintf("%s %s after %s", exitedPrefix, how, lived)
}

// onVMMExit is told by the VMM that the process of a VM ended without a stop
// having asked it to. It only records that: the next poll knows what the control
// plane thinks of the sandbox and reports it (reportExited).
func (r *Reconciler) onVMMExit(id string, info vmm.ExitInfo) {
	r.mu.Lock()
	if r.releasing[id] {
		r.mu.Unlock()
		return // being released: a stop's doing, not a crash
	}
	if r.exits == nil {
		r.exits = make(map[string]vmm.ExitInfo)
	}
	r.exits[id] = info
	r.mu.Unlock()
}

// exitOf is how the VM of id ended, if it ended on its own.
func (r *Reconciler) exitOf(id string) (vmm.ExitInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info, ok := r.exits[id]
	return info, ok
}

// reportExited hands each VM that ended on its own to a worker that reports it,
// and returns those ids: the poll must not start or re-apply anything for them,
// even in the poll that sees the sandbox as running, as it still looks to the
// control plane until the report is in.
//
// A stopping or deleting sandbox is left to its own worker, which tears the VM
// down as it would have. A VM the control plane no longer assigns here is left to
// the fence.
func (r *Reconciler) reportExited(ctx context.Context, work cpclient.Work) map[string]bool {
	r.mu.Lock()
	ended := make(map[string]vmm.ExitInfo, len(r.exits))
	for id, info := range r.exits {
		ended[id] = info
	}
	r.mu.Unlock()
	if len(ended) == 0 {
		return nil
	}
	listed := make(map[string]cpclient.Sandbox, len(work.Sandboxes))
	for _, sb := range work.Sandboxes {
		listed[sb.ID] = sb
	}
	assigned := make(map[string]bool, len(work.Assigned))
	for _, id := range work.Assigned {
		assigned[id] = true
	}
	taken := make(map[string]bool, len(ended))
	for id, info := range ended {
		id, info := id, info
		sb, isListed := listed[id]
		switch {
		case isListed && (sb.State == "stopping" || sb.State == "deleting"):
			continue
		case !isListed && work.Assigned != nil && !assigned[id]:
			continue
		case !isListed:
			// A running sandbox is not listed (only its id is, as assigned) unless
			// it has a local-net session to follow.
			sb = cpclient.Sandbox{ID: id, State: "running"}
		}
		taken[id] = true
		r.dispatch(id, func() {
			if err := r.vmmExited(ctx, sb, info); err != nil {
				r.Logger.Warn("reporting a VM that ended on its own failed; trying again", "sandbox_id", id, "error", err)
			}
		}, func(v any) { r.afterStopPanic(sb, v) })
	}
	return taken
}

// vmmExited reports a sandbox whose VM ended on its own, then releases what the
// VM held. A running sandbox goes to stopped, keeping its disk when the control
// plane keeps stopped sandboxes' disks: the guest saw a power cut, which its
// filesystem recovers from, and its data is what the user cares about. A sandbox
// that was still starting is a start that failed, as for any other (startFailure).
//
// The report comes first. If it cannot be made (the control plane is down) the
// handle and the record stay, and the next poll tries again; teardown first would
// leave a sandbox the control plane believes running with nothing here to say
// otherwise. The control plane refusing the report (409) means it has moved on
// (stopping, deleted, failed over): the VM is gone either way, so it is released.
func (r *Reconciler) vmmExited(ctx context.Context, sb cpclient.Sandbox, info vmm.ExitInfo) error {
	state, detail := "stopped", exitDetail(info)
	if sb.State != "running" {
		state, detail = startFailure(sb, errors.New(detail))
	}
	keep := state == "stopped" && r.retainsDisks()
	line := lastConsoleLine(r.consoleTail(sb.ID))
	r.Logger.Warn("the VM ended on its own", "sandbox_id", sb.ID, "reported_state", state, "detail", detail, "console", strings.TrimSpace(line), "disk_kept", keep)

	if _, err := r.CP.ReportStatus(ctx, sb.ID, state, detail); err != nil && !cpclient.IsConflict(err) {
		return fmt.Errorf("report %s: %w", state, err)
	}
	r.teardownLocal(ctx, sb.ID, teardownOpts{keepDisk: keep})
	return nil
}
