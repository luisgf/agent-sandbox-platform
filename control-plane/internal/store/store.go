package store

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidInput  = errors.New("invalid input")
	ErrAlreadyExists = errors.New("already exists")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrConflict      = errors.New("conflict")
)

// errNodeRevoked is returned when a revoked node registers or heartbeats.
// Only a fresh enroll (new certificate) brings it back (ADR-0005).
func errNodeRevoked(id string) error {
	return fmt.Errorf("%w: node %s is revoked; re-enroll required", ErrConflict, id)
}

// Store is the persistence boundary for the control plane.
// MemoryStore is the default; PostgresStore is used when DATABASE_URL is set.
type Store interface {
	CreateSandbox(input CreateSandboxInput) (Sandbox, error)
	GetSandbox(id string) (Sandbox, error)
	ListSandboxes(tenantID string) ([]Sandbox, error)

	// ClaimSandbox moves a requested sandbox placed on nodeID to starting;
	// ErrConflict when it is placed elsewhere or no longer requested.
	ClaimSandbox(id, nodeID string) (Sandbox, error)
	// ListNodeWork returns, for one node, the sandboxes that need its action
	// and the ids of every sandbox assigned to it (see NodeWork).
	ListNodeWork(nodeID string) (NodeWork, error)
	// UpdateSandboxStatus sets lifecycle state (starting|running|failed|stopped).
	UpdateSandboxStatus(id string, state SandboxState, detail string) (Sandbox, error)
	// MarkSandboxStopping moves an active sandbox to stopping for reconciler cleanup.
	// actorSub is optional (ADR-0007); empty is OK in lab.
	MarkSandboxStopping(id, actorSub string) (Sandbox, error)

	// TouchSandboxActivity records a successful exec (or equivalent) as last_activity_at=now.
	TouchSandboxActivity(id string) error
	// StopIdleSandboxes marks active sandboxes idle longer than idleFor as stopping
	// (or stopped when never assigned). idleFor <= 0 is a no-op (reaper disabled).
	StopIdleSandboxes(now time.Time, idleFor time.Duration) ([]Sandbox, error)

	RegisterNode(input RegisterNodeInput) (Node, error)
	// CreateEnrollToken stores a single-use enroll token by its hash.
	CreateEnrollToken(tok EnrollToken) error
	// CheckEnroll reports whether auth may enroll node id now, without
	// changing anything (EnrollNode checks again atomically). Errors:
	// ErrEnrollTokenInvalid, ErrEnrollTokenPinned, ErrNodeEnrolled.
	CheckEnroll(id string, auth EnrollAuth) error
	// EnrollNode records an enrollment and its certificate. A node id that holds
	// a live certificate needs an enroll token pinned to it; a token is marked
	// used in the same transaction, so two enrollments cannot share it.
	EnrollNode(input EnrollNodeInput, cert CertMeta, auth EnrollAuth) (Node, error)
	// RotateNodeCert issues tracking for a new cert: revokes the previous fingerprint and stores the new meta.
	RotateNodeCert(nodeID string, cert CertMeta) (Node, error)
	// RevokeNode marks the node revoked and adds its current fingerprint to the revocation set.
	RevokeNode(nodeID string) (Node, error)
	// IsCertRevoked reports whether a client-cert fingerprint must be rejected by mTLS middleware.
	IsCertRevoked(fingerprint string) (bool, error)
	HeartbeatNode(id string) (Node, error)
	// TouchNodePoll records that the node polled for work: it is alive. Writes are
	// throttled; ErrNotFound for an unknown node.
	TouchNodePoll(id string, now time.Time) error
	// SetNodeCordoned stops (true) or resumes (false) new placements on a node.
	SetNodeCordoned(id string, cordoned bool) (Node, error)
	// ListNodeUsage sums what is placed on each node (states that hold a node).
	ListNodeUsage() (map[string]NodeUsage, error)
	// MarkNodeOffline marks a silent node offline: only if it is not revoked, not
	// already offline, and has not been seen since silentSince. Reports a change.
	MarkNodeOffline(id string, silentSince time.Time) (bool, error)
	// FailNodeSandboxes fails the sandboxes of a lost node (revoked, or not seen
	// since silentSince, checked atomically): requested through paused → failed,
	// stopping → stopped. A node seen again in between keeps its sandboxes.
	FailNodeSandboxes(nodeID, reason string, silentSince time.Time) ([]Sandbox, error)
	// FailUnassignedRequested fails requested sandboxes without a node created before
	// createdBefore: rows from before placement at create, which no node will claim.
	FailUnassignedRequested(createdBefore time.Time, reason string) ([]Sandbox, error)
	// EmitNodeEvent records a node event (Postgres node_events; the memory store
	// keeps no node events).
	EmitNodeEvent(nodeID, eventType, actor string, payload map[string]any) error
	ListNodes() ([]Node, error)
	GetNode(id string) (Node, error)

	EmitEvent(input EmitEventInput) error
	ListEvents(sandboxID string) ([]SandboxEvent, error)

	LookupAPIKeyByHash(secretHash string) (ApiKey, error)
	// EnsureAPIKey creates or updates the key named name in tenantID with the
	// given scope (APIKeyScopeTenant or APIKeyScopePlatform).
	EnsureAPIKey(tenantID, name, scope, keyPrefix, secretHash string) (ApiKey, error)
	CountAPIKeys() (int64, error)
	TouchAPIKey(id string) error

	ListEgressRules(tenantID string) ([]EgressRule, error)
	PutEgressRules(tenantID string, rules []EgressRule) ([]EgressRule, error)

	PutAttestation(input PutAttestationInput) (AttestationRecord, error)
	GetAttestation(sandboxID string) (AttestationRecord, error)

	// IssueLocalNetGrant returns a clear grant once (only the hash is stored).
	// Refuses when local_net is false. Does not refresh last_activity_at.
	IssueLocalNetGrant(id, dial string, now time.Time, ttl time.Duration) (grant string, expires time.Time, err error)
	// HeartbeatLocalNet moves pending/withdrawn to up. Expired grant withdraws
	// (blackhole) and returns ErrUnauthorized. Does not refresh last_activity_at.
	HeartbeatLocalNet(id, grant, clientPublic string, now time.Time) (Sandbox, error)
	// WithdrawLocalNet detaches. local_net stays true; state becomes withdrawn.
	WithdrawLocalNet(id string) (Sandbox, error)
	// SetLocalNetNodePublic records the node device public key. It does not
	// change local_net or the state, and it does not refresh last_activity_at.
	SetLocalNetNodePublic(id, publicKey string) (Sandbox, error)
}
