package store

import (
	"encoding/json"
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
)

type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Sandbox struct {
	ID             string       `json:"id"`
	TenantID       string       `json:"tenant_id"`
	NodeID         *string      `json:"node_id"`
	State          SandboxState `json:"state"`
	VMMProfile     string       `json:"vmm_profile"`
	ImageRef       string       `json:"image_ref"`
	CPUMillis      int          `json:"cpu_millis"`
	MemoryMiB      int          `json:"memory_mib"`
	StateVersion   int64        `json:"state_version"`
	NodeLeaseUntil *time.Time   `json:"node_lease_until,omitempty"`
	// OwnerSub is the IdP subject of the creator (ADR-0007). Empty OK in lab until IdP JWT.
	OwnerSub string `json:"owner_sub,omitempty"`
	// OwnerEmail is optional ops/UI claim; not an authz key.
	OwnerEmail string `json:"owner_email,omitempty"`
	// LastActivityAt is the last successful exec, or create/start (transition to running).
	// Lease renew and heartbeats do not move it. Idle reaping compares it to the threshold.
	LastActivityAt time.Time `json:"last_activity_at"`
	// StopReason is set by the idle reaper (idle_timeout). Empty otherwise.
	StopReason string `json:"stop_reason,omitempty"`
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
	// LocalNetGrantHash is sha256 hex of the live grant. Never serialized.
	LocalNetGrantHash string    `json:"-"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type Node struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Endpoint        string     `json:"endpoint"`
	AgentEndpoint   string     `json:"agent_endpoint,omitempty"`
	State           string     `json:"state"`
	VMMProfiles     []string   `json:"vmm_profiles"`
	CapacityCPU     int        `json:"capacity_cpu"`
	CapacityMemMiB  int        `json:"capacity_mem_mib"`
	CertFingerprint string     `json:"cert_fingerprint,omitempty"`
	CertSerial      string     `json:"cert_serial,omitempty"`
	FenceToken      string     `json:"fence_token,omitempty"`
	FenceEndpoint   string     `json:"fence_endpoint,omitempty"`
	EnrolledAt      *time.Time `json:"enrolled_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type ApiKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
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
	FenceEndpoint  string   `json:"fence_endpoint,omitempty"`
	FenceToken     string   `json:"fence_token,omitempty"`
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
