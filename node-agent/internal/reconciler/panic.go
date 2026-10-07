package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
)

// panicCleanupTimeout bounds the cleanup after a panic: the VMM and the control
// plane are called, and either may be what is wrong.
const panicCleanupTimeout = 30 * time.Second

// panicDetail is the status detail a sandbox gets when its worker panicked.
func panicDetail(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 200 {
		s = s[:200]
	}
	return "panic: " + s
}

// afterStartPanic frees what the start of sb held on this node and reports it as
// a start that failed: failed for a first boot, stopped (disk kept, can be
// resumed) for a resume, as for any other failed start. What the start had not
// yet recorded (the TAP and the CID it was allocating) is released by the next
// agent start.
func (r *Reconciler) afterStartPanic(sb cpclient.Sandbox, v any) {
	ctx, cancel := context.WithTimeout(context.Background(), panicCleanupTimeout)
	defer cancel()
	resumed := sb.BootedAt != nil || sb.BootCount > 1
	r.teardownLocal(ctx, sb.ID, teardownOpts{keepDisk: resumed, how: "panic"})
	if !resumed {
		r.removeRootFSByID(sb.ID)
	}
	state, _ := startFailure(sb, fmt.Errorf("%s", panicDetail(v)))
	if _, err := r.CP.ReportStatus(ctx, sb.ID, state, panicDetail(v)); err != nil {
		r.Logger.Warn("report after panic", "sandbox_id", sb.ID, "state", state, "error", err)
	}
}

// afterStopPanic finishes a stop that panicked: whatever was left of the VM is
// torn down (the disk kept when the control plane retains disks) and the sandbox
// reported stopped, so it can be resumed.
func (r *Reconciler) afterStopPanic(sb cpclient.Sandbox, v any) {
	ctx, cancel := context.WithTimeout(context.Background(), panicCleanupTimeout)
	defer cancel()
	r.teardownLocal(ctx, sb.ID, teardownOpts{keepDisk: r.retainsDisks(), how: "panic"})
	if _, err := r.CP.ReportStatus(ctx, sb.ID, "stopped", panicDetail(v)); err != nil {
		r.Logger.Warn("report after panic", "sandbox_id", sb.ID, "state", "stopped", "error", err)
	}
}

// afterDeletePanic finishes a delete that panicked: the VM and the disk are
// removed as far as they can be and the sandbox reported deleted. A leftover disk
// is not owned by any sandbox any more, so the disk GC takes it.
func (r *Reconciler) afterDeletePanic(sb cpclient.Sandbox, v any) {
	ctx, cancel := context.WithTimeout(context.Background(), panicCleanupTimeout)
	defer cancel()
	r.teardownLocal(ctx, sb.ID, teardownOpts{how: "panic"})
	r.removeRootFSByID(sb.ID)
	if _, err := r.CP.ReportStatus(ctx, sb.ID, "deleted", panicDetail(v)); err != nil {
		r.Logger.Warn("report after panic", "sandbox_id", sb.ID, "state", "deleted", "error", err)
	}
}
