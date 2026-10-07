package store

import (
	"encoding/json"
	"fmt"
	"time"
)

type SandboxState string

const (
	SandboxRequested SandboxState = "requested"
	SandboxScheduled SandboxState = "scheduled"
	SandboxStarting  SandboxState = "starting"
	SandboxRunning   SandboxState = "running"
	SandboxPaused    SandboxState = "paused"
	SandboxStopping  SandboxState = "stopping"
	SandboxStopped   SandboxState = "stopped"
	SandboxFailed    SandboxState = "failed"
	// SandboxDeleting: the node is removing the VM and the disk (ADR-0012).
	SandboxDeleting SandboxState = "deleting"
	// SandboxDeleted is final. The row stays, so the audit trail does.
	SandboxDeleted SandboxState = "deleted"
)

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Sandbox struct {
	ID           string       `json:"id"`
	TenantID     string       `json:"tenant_id"`
	NodeID       *string      `json:"node_id"`
	State        SandboxState `json:"state"`
	VMMProfile   string       `json:"vmm_profile"`
	ImageRef     string       `json:"image_ref"`
	CPUMillis    int          `json:"cpu_millis"`
	MemoryMiB    int          `json:"memory_mib"`
	StateVersion int64        `json:"state_version"`
	// OwnerSub is the IdP subject of the creator (ADR-0007). Empty OK in lab until IdP JWT.
	OwnerSub string `json:"owner_sub,omitempty"`
	// OwnerEmail is optional ops/UI claim; not an authz key.
	OwnerEmail string `json:"owner_email,omitempty"`
	// LastActivityAt is the last successful exec, or create/start (transition to running).
	// Lease renew and heartbeats do not move it. Idle reaping compares it to the threshold.
	LastActivityAt time.Time `json:"last_activity_at"`
	// StopReason is set by the idle reaper (idle_timeout). Empty otherwise.
	StopReason string `json:"stop_reason,omitempty"`
	// StatusDetail is why a node last reported failed, or a resume went back to
	// stopped. Cleared when the sandbox runs or is resumed.
	StatusDetail string `json:"status_detail,omitempty"`
	// BootCount is how many times the sandbox has been started: 1 at the first
	// boot, one more per resume. It is a counter to show; it does not say whether
	// a disk exists (BootedAt does).
	BootCount int `json:"boot_count"`
	// BootedAt is when the node first reported the sandbox running. Until then no
	// disk is worth keeping: a sandbox stopped before it ever ran resumes on a
	// fresh disk, while one that ran resumes on the disk its stop kept, and its
	// node reports it lost if that disk is gone. Never cleared.
	BootedAt *time.Time `json:"booted_at,omitempty"`
	// StoppedAt is when the node reported the sandbox stopped; nil while it is
	// not stopped. The retention TTL counts from it.
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// WorkspaceHostPath is the host directory the session asked to share into
	// the guest (virtiofs tag "workspace", mount /workspace). Empty means no
	// share. The node-agent starts virtiofsd when this is set. A guest image
	// with workspace-virtiofs.service mounts the tag at boot; an older image
	// still needs mount -t virtiofs.
	WorkspaceHostPath string `json:"workspace_host_path,omitempty"`
	// LocalNet is the full-tunnel opt-in (ADR-0010). Default false.
	// When true, this sandbox's default route is the local-agent tunnel,
	// never the node public proxy — including while the tunnel is down.
	LocalNet bool `json:"local_net"`
	// LocalNetState is off | pending | up | withdrawn.
	LocalNetState string `json:"local_net_state"`
	// LocalNetAttachedAt is the last transition to up. Not a secret.
	LocalNetAttachedAt *time.Time `json:"local_net_attached_at,omitempty"`
	// LocalNetGrantExpiresAt is the current grant deadline. The grant itself is not stored.
	LocalNetGrantExpiresAt *time.Time `json:"local_net_grant_expires_at,omitempty"`
	// LocalNetClientPublic is the agent's WireGuard public key (not a secret).
	LocalNetClientPublic string `json:"local_net_client_public,omitempty"`
	// LocalNetNodePublic is the node WireGuard public key for this sandbox (not a secret).
	LocalNetNodePublic string `json:"local_net_node_public,omitempty"`
	// LocalNetListenPort, LocalNetNodeAddr and LocalNetClientAddr are what the
	// node allocated for the tunnel (017): its device's UDP port and the two
	// ends of the /30. Zero until the node publishes them.
	LocalNetListenPort int    `json:"local_net_listen_port,omitempty"`
	LocalNetNodeAddr   string `json:"local_net_node_addr,omitempty"`
	LocalNetClientAddr string `json:"local_net_client_addr,omitempty"`
	// LocalNetGrantHash is sha256 hex of the live grant. Never serialized.
	LocalNetGrantHash string    `json:"-"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Node struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Endpoint       string   `json:"endpoint"`
	AgentEndpoint  string   `json:"agent_endpoint,omitempty"`
	State          string   `json:"state"`
	VMMProfiles    []string `json:"vmm_profiles"`
	CapacityCPU    int      `json:"capacity_cpu"`
	CapacityMemMiB int      `json:"capacity_mem_mib"`
	// MaxSandboxes caps sandboxes on the node; 0 = no limit. A 0 capacity_cpu or
	// capacity_mem_mib is not enforced either (ADR-0011).
	MaxSandboxes int `json:"max_sandboxes"`
	// Cordoned: an admin stopped new placements; running sandboxes stay.
	Cordoned bool `json:"cordoned"`
	// AcceptsWork is false for agents running without --reconcile.
	AcceptsWork bool `json:"accepts_work"`
	// LocalNetDial is the host[:port] a laptop dials for this node's local-net tunnels.
	LocalNetDial string `json:"local_net_dial,omitempty"`
	// EgressEnforced: the node forces its guests through its egress proxy, with
	// nft rules applied in enforce mode, so a tenant's egress policy binds them.
	// Reported by the node on register (021).
	EgressEnforced bool `json:"egress_enforced"`
	// AgentInstanceID changes when the node-agent process restarts.
	AgentInstanceID string `json:"agent_instance_id,omitempty"`
	CertFingerprint string `json:"cert_fingerprint,omitempty"`
	CertSerial      string `json:"cert_serial,omitempty"`
	// CertNotAfter is when the current node certificate expires (016).
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
	// DiskFreeMiB is the free space of the node's --disk-dir at its last
	// heartbeat (019). Nil until a node that reports it has heartbeated.
	DiskFreeMiB *int64 `json:"disk_free_mib,omitempty"`
	// Fence credentials can power the node off; they never leave the control plane.
	FenceToken    string     `json:"-"`
	FenceEndpoint string     `json:"-"`
	EnrolledAt    *time.Time `json:"enrolled_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// API key scopes: a tenant key acts only within its tenant; a platform key
// (operations tooling, the bootstrap key) sees every tenant.
const (
	APIKeyScopeTenant   = "tenant"
	APIKeyScopePlatform = "platform"
)

type ApiKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	KeyPrefix  string     `json:"key_prefix"`
	SecretHash string     `json:"-"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// SandboxEvent is an append-only audit row for sandbox lifecycle.
type SandboxEvent struct {
	ID        int64   `json:"id"`
	SandboxID string  `json:"sandbox_id"`
	TenantID  string  `json:"tenant_id"`
	EventType string  `json:"event_type"`
	FromState *string `json:"from_state,omitempty"`
	ToState   *string `json:"to_state,omitempty"`
	Actor     string  `json:"actor"`
	// ActorSub is the human/service principal for this action (ADR-0007 phase 1). Empty OK in lab.
	ActorSub  string          `json:"actor_sub,omitempty"`
	RequestID *string         `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// EmitEventInput is the payload for appending a sandbox event.
type EmitEventInput struct {
	SandboxID string
	TenantID  string
	EventType string
	FromState *string
	ToState   *string
	Actor     string
	ActorSub  string
	RequestID *string
	Payload   json.RawMessage
}

// CreateSandboxInput is the validated payload for creating a sandbox.
type CreateSandboxInput struct {
	TenantID   string `json:"tenant_id"`
	ImageRef   string `json:"image_ref"`
	CPUMillis  int    `json:"cpu_millis"`
	MemoryMiB  int    `json:"memory_mib"`
	VMMProfile string `json:"vmm_profile"`
	// NodeID optionally pins the provisioner stub to a specific node (MVP dry-run).
	NodeID string `json:"node_id,omitempty"`
	// OwnerSub / OwnerEmail: lab/transition accepts from JSON; phase 2+ come from IdP JWT.
	OwnerSub   string `json:"owner_sub,omitempty"`
	OwnerEmail string `json:"owner_email,omitempty"`
	// ActorSub is who performs create now (header X-ASP-Actor-Sub preferred; body ok; falls back to OwnerSub).
	ActorSub string `json:"actor_sub,omitempty"`
	// WorkspaceHostPath is an absolute host directory to share (optional).
	// The control plane stores it; it does not mount it.
	WorkspaceHostPath string `json:"workspace_host_path,omitempty"`
	// LocalNet opts this sandbox into the full-tunnel default route (ADR-0010).
	// Nil or false keeps the node public egress. There is no per-CIDR field in v1.
	LocalNet *bool `json:"local_net,omitempty"`
}

// RegisterNodeInput is the payload for node-agent registration.
type RegisterNodeInput struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Endpoint       string   `json:"endpoint"`
	AgentEndpoint  string   `json:"agent_endpoint"`
	VMMProfiles    []string `json:"vmm_profiles"`
	CapacityCPU    int      `json:"capacity_cpu"`
	CapacityMemMiB int      `json:"capacity_mem_mib"`
	MaxSandboxes   int      `json:"max_sandboxes"`
	// AcceptsWork: nil (agents that predate the field) means true.
	AcceptsWork  *bool  `json:"accepts_work,omitempty"`
	LocalNetDial string `json:"local_net_dial,omitempty"`
	// EgressEnforced is true when the node applies its egress proxy and nft
	// rules in enforce mode. Absent (an older node) means false.
	EgressEnforced bool `json:"egress_enforced,omitempty"`
	// AgentInstanceID is random per node-agent process; a new one on register
	// means the agent restarted and its running sandboxes are orphaned.
	AgentInstanceID string `json:"agent_instance_id,omitempty"`
}

// acceptsWork resolves the optional register field.
func (in RegisterNodeInput) acceptsWork() bool {
	return in.AcceptsWork == nil || *in.AcceptsWork
}

// validateNodeCapacity rejects negative capacity; 0 means "not enforced".
func validateNodeCapacity(cpu, memMiB, maxSandboxes int) error {
	if cpu < 0 || memMiB < 0 || maxSandboxes < 0 {
		return fmt.Errorf("%w: capacity_cpu, capacity_mem_mib and max_sandboxes must be >= 0", ErrInvalidInput)
	}
	return nil
}

// EnrollNodeInput is the payload for bootstrap-token enrollment.
type EnrollNodeInput struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Endpoint       string   `json:"endpoint"`
	AgentEndpoint  string   `json:"agent_endpoint"`
	VMMProfiles    []string `json:"vmm_profiles"`
	CapacityCPU    int      `json:"capacity_cpu"`
	CapacityMemMiB int      `json:"capacity_mem_mib"`
}

// EnrollNodeResult is persisted enrollment plus cert metadata for the API response.
type EnrollNodeResult struct {
	Node            Node
	CertFingerprint string
}

// CertMeta is the SHA-256 fingerprint + serial of a node client certificate.
type CertMeta struct {
	Fingerprint string
	Serial      string
	NotAfter    time.Time // zero: unknown
}

// notAfterPtr returns the expiry to store, nil when unknown.
func (c CertMeta) notAfterPtr() *time.Time {
	if c.NotAfter.IsZero() {
		return nil
	}
	t := c.NotAfter.UTC()
	return &t
}

// EgressRule is one allowlist entry for a tenant.
type EgressRule struct {
	ID          string `json:"id,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	HostPattern string `json:"host_pattern"`
	Port        *int   `json:"port,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// PutEgressInput replaces the full rule set for a tenant.
type PutEgressInput struct {
	Rules []EgressRule `json:"rules"`
}

// EgressPolicy is the effective allowlist attached to node-agent calls.
// Mode is "allow-all" or "deny-default".
type EgressPolicy struct {
	TenantID string       `json:"tenant_id"`
	Mode     string       `json:"mode"` // allow-all | deny-default
	Rules    []EgressRule `json:"rules"`
}

// AttestationRecord is the latest verified boot evidence for a sandbox.
type AttestationRecord struct {
	SandboxID   string          `json:"sandbox_id"`
	NodeID      string          `json:"node_id"`
	ImageDigest string          `json:"image_digest"`
	VMMProfile  string          `json:"vmm_profile"`
	CID         uint32          `json:"cid"`
	StatementTS time.Time       `json:"statement_ts"`
	Alg         string          `json:"alg"`
	KeyID       string          `json:"key_id,omitempty"`
	Signature   string          `json:"signature"`
	Bundle      json.RawMessage `json:"bundle"`
	ReceivedAt  time.Time       `json:"received_at"`
}

// PutAttestationInput is the verified evidence to persist.
type PutAttestationInput struct {
	SandboxID   string
	NodeID      string
	ImageDigest string
	VMMProfile  string
	CID         uint32
	StatementTS time.Time
	Alg         string
	KeyID       string
	Signature   string
	Bundle      json.RawMessage
}
