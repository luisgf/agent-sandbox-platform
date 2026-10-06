package sshagent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ErrSandboxRequired is returned by Approve for an approval without a sandbox
// id when the Approver does not allow global approvals.
var ErrSandboxRequired = errors.New("sandbox_id required: approvals are scoped to the sandbox that will sign")

// Approver holds one-shot approvals for SIGN_REQUEST (--ssh-agent-confirm).
// Each approval belongs to one sandbox and is consumed by the next sign that
// arrives through that sandbox's listener, or expires.
type Approver struct {
	DefaultTTL time.Duration
	// GlobalApprovals allows approvals without a sandbox id. Only listeners
	// that cannot tell guests apart (--ssh-agent-bridge, the global host-vsock
	// listener) consume them, so whichever guest reaches those listeners
	// first uses them (lab only: --insecure-ssh-agent-global-approvals).
	// Without it, those listeners deny every sign while the gate is on.
	GlobalApprovals bool

	mu      sync.Mutex
	pending map[string]approval // approval id → approval
}

type approval struct {
	sandboxID string // "" = global approval
	expires   time.Time
}

// NewApprover creates an empty Approver with default TTL (30s if unset).
func NewApprover(ttl time.Duration) *Approver {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Approver{pending: make(map[string]approval), DefaultTTL: ttl}
}

// Approve registers a one-shot approval for the next sign from sandboxID,
// valid for ttl (or DefaultTTL). An empty sandboxID asks for a global
// approval, which needs GlobalApprovals. The returned id is for the audit
// log only: nothing has to present it.
func (a *Approver) Approve(sandboxID string, ttl time.Duration) (id string, expires time.Time, err error) {
	if a == nil {
		return "", time.Time{}, errors.New("ssh-agent confirmation gate not enabled")
	}
	if sandboxID == "" && !a.GlobalApprovals {
		return "", time.Time{}, ErrSandboxRequired
	}
	if ttl <= 0 {
		ttl = a.DefaultTTL
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id = hex.EncodeToString(b[:])
	now := time.Now().UTC()
	expires = now.Add(ttl)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked(now)
	if a.pending == nil {
		a.pending = make(map[string]approval)
	}
	a.pending[id] = approval{sandboxID: sandboxID, expires: expires}
	return id, expires, nil
}

// PendingCount returns the approvals for sandboxID that have not expired.
func (a *Approver) PendingCount(sandboxID string) int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked(time.Now().UTC())
	n := 0
	for _, ap := range a.pending {
		if ap.sandboxID == sandboxID {
			n++
		}
	}
	return n
}

// Consume uses one approval for a sign from sandboxID ("" for a listener that
// cannot tell guests apart). It returns false when there is none, which
// denies the sign.
func (a *Approver) Consume(sandboxID string) bool {
	_, ok := a.consume(sandboxID)
	return ok
}

// consume takes the approval for sandboxID that expires first and returns its
// id. An approval only unlocks a sign from its own sandbox; a global one only
// a sign from a listener without a sandbox.
func (a *Approver) consume(sandboxID string) (string, bool) {
	if a == nil || (sandboxID == "" && !a.GlobalApprovals) {
		return "", false
	}
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gcLocked(now)
	var (
		bestID string
		best   approval
	)
	for id, ap := range a.pending {
		if ap.sandboxID != sandboxID {
			continue
		}
		if bestID == "" || ap.expires.Before(best.expires) {
			bestID, best = id, ap
		}
	}
	if bestID == "" {
		return "", false
	}
	delete(a.pending, bestID)
	return bestID, true
}

func (a *Approver) gcLocked(now time.Time) {
	for id, ap := range a.pending {
		if !ap.expires.After(now) {
			delete(a.pending, id)
		}
	}
}
