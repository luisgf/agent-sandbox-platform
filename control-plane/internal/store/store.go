package store

import (
	"context"
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
// MemoryStore is the default; PostgresStore is used when ASP_DATABASE_URL is set.
//
// The two are one contract, and parity_test.go holds them to it: it plays a script
// with a step for every method below against both and requires the same results and
// the same class of error. A method added here needs a step there.
type Store interface {
	CreateSandbox(ctx context.Context, input CreateSandboxInput) (Sandbox, error)
	GetSandbox(ctx context.Context, id string) (Sandbox, error)
	ListSandboxes(ctx context.Context, tenantID string) ([]Sandbox, error)

	// ClaimSandbox moves a requested sandbox placed on nodeID to starting;
	// ErrConflict when it is placed elsewhere or no longer requested.
	ClaimSandbox(ctx context.Context, id, nodeID string) (Sandbox, error)
	// ListNodeWork returns, for one node, the sandboxes that need its action
	// and the ids of every sandbox assigned to it (see NodeWork).
	ListNodeWork(ctx context.Context, nodeID string) (NodeWork, error)
	// UpdateSandboxStatus sets lifecycle state (starting|running|failed|stopped|deleted).
	UpdateSandboxStatus(ctx context.Context, id string, state SandboxState, detail string) (Sandbox, error)
	// StopSandbox stops an active sandbox and keeps its disk (ADR-0012):
	// stopping for its node to power it off, or stopped at once when it was never
	// claimed. Stopped or stopping already is not an error; failed, deleting and
	// deleted are ErrConflict. actorSub is optional (ADR-0007).
	StopSandbox(ctx context.Context, id, actorSub string) (Sandbox, error)
	// ResumeSandbox starts a stopped sandbox again on the node that holds its
	// disk: placement is pinned to it (ErrNoCapacity, ErrNodeUnavailable), then
	// stopped becomes requested with boot_count + 1. Running, starting or
	// requested already is not an error; any other state is ErrConflict.
	ResumeSandbox(ctx context.Context, id, actorSub string) (Sandbox, error)
	// DeleteSandbox deletes a sandbox and its disk: deleting for its node to do
	// it, or deleted at once when no node holds anything (never claimed, failed,
	// or stopped without a node). Deleting or deleted already is not an error.
	DeleteSandbox(ctx context.Context, id, actorSub string) (Sandbox, error)

	// TouchSandboxActivity records a successful exec (or equivalent) as last_activity_at=now.
	TouchSandboxActivity(ctx context.Context, id string) error
	// StopIdleSandboxes marks active sandboxes idle longer than idleFor as stopping
	// (or stopped when never assigned). idleFor <= 0 is a no-op (reaper disabled).
	StopIdleSandboxes(ctx context.Context, now time.Time, idleFor time.Duration) ([]Sandbox, error)

	// ExpireStoppedSandboxes deletes the sandboxes stopped for at least ttl: deleting
	// for their node to remove the disk, or deleted when no node holds one, with
	// stop_reason retention_expired. ttl <= 0 is a no-op (kept until deleted).
	ExpireStoppedSandboxes(ctx context.Context, now time.Time, ttl time.Duration) ([]Sandbox, error)
	// EvictStoppedOverCap deletes, per tenant, the oldest stopped sandboxes beyond
	// max (stop_reason tenant_cap). max <= 0 is a no-op.
	EvictStoppedOverCap(ctx context.Context, max int) ([]Sandbox, error)

	RegisterNode(ctx context.Context, input RegisterNodeInput) (Node, error)
	// CreateEnrollToken stores a single-use enroll token by its hash.
	CreateEnrollToken(ctx context.Context, tok EnrollToken) error
	// CheckEnroll reports whether auth may enroll node id now, without
	// changing anything (EnrollNode checks again atomically). Errors:
	// ErrEnrollTokenInvalid, ErrEnrollTokenPinned, ErrNodeEnrolled.
	CheckEnroll(ctx context.Context, id string, auth EnrollAuth) error
	// EnrollNode records an enrollment and its certificate. A node id that holds
	// a live certificate needs an enroll token pinned to it; a token is marked
	// used in the same transaction, so two enrollments cannot share it.
	EnrollNode(ctx context.Context, input EnrollNodeInput, cert CertMeta, auth EnrollAuth) (Node, error)
	// RotateNodeCert issues tracking for a new cert: revokes the previous fingerprint and stores the new meta.
	RotateNodeCert(ctx context.Context, nodeID string, cert CertMeta) (Node, error)
	// RevokeNode marks the node revoked and adds its current fingerprint to the revocation set.
	RevokeNode(ctx context.Context, nodeID string) (Node, error)
	// IsCertRevoked reports whether a client-cert fingerprint must be rejected by mTLS middleware.
	IsCertRevoked(ctx context.Context, fingerprint string) (bool, error)
	HeartbeatNode(ctx context.Context, id string) (Node, error)
	// SetNodeDiskFree records the free space of the node's --disk-dir, as it
	// reported it on a heartbeat. ErrNotFound for an unknown node.
	SetNodeDiskFree(ctx context.Context, id string, freeMiB int64) error
	// CountSandboxes counts the sandboxes of each tenant by state, deleted ones
	// included, for the metrics: one grouped query, not a listing.
	CountSandboxes(ctx context.Context) ([]SandboxCount, error)
	// CountStoppedByNode counts the stopped sandboxes (the disks a stop keeps) on each node.
	CountStoppedByNode(ctx context.Context) (map[string]int64, error)
	// TouchNodePoll records that the node polled for work: it is alive. Writes are
	// throttled; ErrNotFound for an unknown node.
	TouchNodePoll(ctx context.Context, id string, now time.Time) error
	// SetNodeCordoned stops (true) or resumes (false) new placements on a node.
	SetNodeCordoned(ctx context.Context, id string, cordoned bool) (Node, error)
	// SetNodeFence sets, or with an empty endpoint clears, the power-off target
	// the control plane uses when it declares the node lost. It is an operator
	// decision: a node never chooses it, or a compromised one could have the
	// control plane power off another host. ErrNotFound for an unknown node.
	SetNodeFence(ctx context.Context, id, endpoint, token string) (Node, error)
	// ListNodeUsage sums what is placed on each node (states that hold a node).
	ListNodeUsage(ctx context.Context) (map[string]NodeUsage, error)
	// MarkNodeOffline marks a silent node offline: only if it is not revoked, not
	// already offline, and has not been seen since silentSince. Reports a change.
	MarkNodeOffline(ctx context.Context, id string, silentSince time.Time) (bool, error)
	// FailNodeSandboxes fails the sandboxes of a lost node (revoked, or not seen
	// since silentSince, checked atomically): requested through paused → failed,
	// stopping → stopped. A node seen again in between keeps its sandboxes.
	FailNodeSandboxes(ctx context.Context, nodeID, reason string, silentSince time.Time) ([]Sandbox, error)
	// FailUnassignedRequested fails requested sandboxes without a node created before
	// createdBefore: rows from before placement at create, which no node will claim.
	FailUnassignedRequested(ctx context.Context, createdBefore time.Time, reason string) ([]Sandbox, error)
	// EmitNodeEvent records a node event (Postgres node_events; the memory store
	// keeps no node events).
	EmitNodeEvent(ctx context.Context, nodeID, eventType, actor string, payload map[string]any) error
	ListNodes(ctx context.Context) ([]Node, error)
	GetNode(ctx context.Context, id string) (Node, error)

	EmitEvent(ctx context.Context, input EmitEventInput) error
	ListEvents(ctx context.Context, sandboxID string) ([]SandboxEvent, error)

	LookupAPIKeyByHash(ctx context.Context, secretHash string) (ApiKey, error)
	// EnsureAPIKey creates or updates the key named name in tenantID with the
	// given scope (APIKeyScopeTenant or APIKeyScopePlatform).
	EnsureAPIKey(ctx context.Context, tenantID, name, scope, keyPrefix, secretHash string) (ApiKey, error)
	CountAPIKeys(ctx context.Context) (int64, error)
	TouchAPIKey(ctx context.Context, id string) error
	// CreateAPIKey adds a key. ErrConflict when the tenant already has a key
	// with that name or another key has that prefix (the caller picks a new
	// secret and tries again).
	CreateAPIKey(ctx context.Context, tenantID, name, scope, keyPrefix, secretHash string, expiresAt *time.Time) (ApiKey, error)
	// ListAPIKeys returns the keys of a tenant, or of every tenant when tenantID
	// is empty, revoked ones included. Never a secret or its hash.
	ListAPIKeys(ctx context.Context, tenantID string) ([]ApiKey, error)
	// GetAPIKey returns one key by id; ErrNotFound if there is none.
	GetAPIKey(ctx context.Context, id string) (ApiKey, error)
	// RevokeAPIKey stops a key from authenticating; revoking twice is a no-op.
	RevokeAPIKey(ctx context.Context, id string) (ApiKey, error)
	// RotateAPIKey gives a key a new secret: the old one stops working at once.
	// ErrConflict for a revoked key or a taken prefix.
	RotateAPIKey(ctx context.Context, id, keyPrefix, secretHash string) (ApiKey, error)

	ListEgressRules(ctx context.Context, tenantID string) ([]EgressRule, error)
	// ListEgressRulesForTenants returns the rules of several tenants in one
	// read (every tenant in tenantIDs has an entry, possibly empty).
	ListEgressRulesForTenants(ctx context.Context, tenantIDs []string) (map[string][]EgressRule, error)
	PutEgressRules(ctx context.Context, tenantID string, rules []EgressRule) ([]EgressRule, error)

	PutAttestation(ctx context.Context, input PutAttestationInput) (AttestationRecord, error)
	GetAttestation(ctx context.Context, sandboxID string) (AttestationRecord, error)

	// IssueLocalNetGrant returns a clear grant once (only the hash is stored).
	// Refuses when local_net is false. Does not refresh last_activity_at.
	IssueLocalNetGrant(ctx context.Context, id, dial string, now time.Time, ttl time.Duration) (grant string, expires time.Time, err error)
	// HeartbeatLocalNet moves pending/withdrawn to up. Expired grant withdraws
	// (blackhole) and returns ErrUnauthorized. Does not refresh last_activity_at.
	HeartbeatLocalNet(ctx context.Context, id, grant, clientPublic string, now time.Time) (Sandbox, error)
	// WithdrawLocalNet detaches. local_net stays true; state becomes withdrawn.
	WithdrawLocalNet(ctx context.Context, id string) (Sandbox, error)
	// SetLocalNetNodePublic records the node device public key. It does not
	// change local_net or the state, and it does not refresh last_activity_at.
	SetLocalNetNodePublic(ctx context.Context, id, publicKey string, tun LocalNetTunnel) (Sandbox, error)
}
