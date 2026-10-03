package store

import (
	"errors"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidInput  = errors.New("invalid input")
	ErrAlreadyExists = errors.New("already exists")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrConflict      = errors.New("conflict")
)

// Store is the persistence boundary for the control plane.
// MemoryStore is the default; PostgresStore is used when DATABASE_URL is set.
type Store interface {
	CreateSandbox(input CreateSandboxInput) (Sandbox, error)
	GetSandbox(id string) (Sandbox, error)
	ListSandboxes(tenantID string) ([]Sandbox, error)
	AssignSandbox(id, nodeID string, state SandboxState) (Sandbox, error)

	// ClaimSandbox atomically assigns a requested sandbox to nodeID.
	// Succeeds when state=requested and (unassigned or already assigned to nodeID).
	// Sets node_id and transitions to starting.
	ClaimSandbox(id, nodeID string) (Sandbox, error)
	// ListNodeWork returns sandboxes this node should act on:
	// assigned requested/starting/stopping, or unassigned requested.
	ListNodeWork(nodeID string) ([]Sandbox, error)
	// UpdateSandboxStatus sets lifecycle state (starting|running|failed|stopped).
	UpdateSandboxStatus(id string, state SandboxState, detail string) (Sandbox, error)
	// MarkSandboxStopping moves an active sandbox to stopping for reconciler cleanup.
	// actorSub is optional (ADR-0007); empty is OK in lab.
	MarkSandboxStopping(id, actorSub string) (Sandbox, error)
	// RenewSandboxLease extends node_lease_until for the owning node.
	RenewSandboxLease(id, nodeID string) (Sandbox, error)
	// ReclaimExpiredLeases marks stuck starting|running with expired leases as failed
	// (or requested when reRequest is true) and clears the assignment so another node may claim.
	ReclaimExpiredLeases(now time.Time, reRequest bool) ([]Sandbox, error)

	// TouchSandboxActivity records a successful exec (or equivalent) as last_activity_at=now.
	TouchSandboxActivity(id string) error
	// StopIdleSandboxes marks active sandboxes idle longer than idleFor as stopping
	// (or stopped when never assigned). idleFor <= 0 is a no-op (reaper disabled).
	StopIdleSandboxes(now time.Time, idleFor time.Duration) ([]Sandbox, error)

	RegisterNode(input RegisterNodeInput) (Node, error)
	EnrollNode(input EnrollNodeInput, cert CertMeta) (Node, error)
	// RotateNodeCert issues tracking for a new cert: revokes the previous fingerprint and stores the new meta.
	RotateNodeCert(nodeID string, cert CertMeta) (Node, error)
	// RevokeNode marks the node revoked and adds its current fingerprint to the revocation set.
	RevokeNode(nodeID string) (Node, error)
	// IsCertRevoked reports whether a client-cert fingerprint must be rejected by mTLS middleware.
	IsCertRevoked(fingerprint string) (bool, error)
	HeartbeatNode(id string) (Node, error)
	ListNodes() ([]Node, error)
	GetNode(id string) (Node, error)

	EmitEvent(input EmitEventInput) error
	ListEvents(sandboxID string) ([]SandboxEvent, error)

	LookupAPIKeyByHash(secretHash string) (ApiKey, error)
	EnsureAPIKey(tenantID, name, keyPrefix, secretHash string) (ApiKey, error)
	CountAPIKeys() (int64, error)
	TouchAPIKey(id string) error

	ListEgressRules(tenantID string) ([]EgressRule, error)
	PutEgressRules(tenantID string, rules []EgressRule) ([]EgressRule, error)

	PutAttestation(input PutAttestationInput) (AttestationRecord, error)
	GetAttestation(sandboxID string) (AttestationRecord, error)
}
