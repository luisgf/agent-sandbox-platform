package store

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
)

const DefaultLocalNodeID = "local-dev"

// MemoryStore is a thread-safe in-memory Store for the MVP / offline tests.
type MemoryStore struct {
	mu           sync.RWMutex
	sandboxes    map[string]Sandbox
	nodes        map[string]Node
	events       []SandboxEvent
	apiKeys      map[string]ApiKey       // keyed by secret hash
	egress       map[string][]EgressRule // tenant_id -> rules
	attestations map[string]AttestationRecord
	revokedCerts map[string]string // fingerprint -> node_id
	nextEvt      int64
	// provisionNodeID is assigned by the stub provisioner when creating sandboxes.
	provisionNodeID string
	schedCfg        sched.Config
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sandboxes:       make(map[string]Sandbox),
		nodes:           make(map[string]Node),
		apiKeys:         make(map[string]ApiKey),
		egress:          make(map[string][]EgressRule),
		attestations:    make(map[string]AttestationRecord),
		revokedCerts:    make(map[string]string),
		events:          make([]SandboxEvent, 0),
		provisionNodeID: DefaultLocalNodeID,
		schedCfg:        sched.DefaultConfig(),
	}
}

// SetSchedConfig sets the placement policy (ASP_SCHED_POLICY and friends).
func (m *MemoryStore) SetSchedConfig(cfg sched.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedCfg = cfg
}

// nodeUsageLocked sums what is placed on each node. Caller holds m.mu.
func (m *MemoryStore) nodeUsageLocked() map[string]NodeUsage {
	usage := make(map[string]NodeUsage, len(m.nodes))
	for _, sb := range m.sandboxes {
		if sb.NodeID == nil || *sb.NodeID == "" || !OccupiesNode(sb.State) {
			continue
		}
		u := usage[*sb.NodeID]
		u.add(sb)
		usage[*sb.NodeID] = u
	}
	return usage
}

// SetProvisionNodeID overrides the stub node used by sync provisioning.
func (m *MemoryStore) SetProvisionNodeID(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provisionNodeID = id
}

func (m *MemoryStore) CreateSandbox(input CreateSandboxInput) (Sandbox, error) {
	if err := prepareCreateSandbox(&input); err != nil {
		return Sandbox{}, err
	}
	now := time.Now().UTC()
	ownerSub := strings.TrimSpace(input.OwnerSub)
	ownerEmail := strings.TrimSpace(input.OwnerEmail)
	actorSub := strings.TrimSpace(input.ActorSub)
	if actorSub == "" {
		actorSub = ownerSub
	}
	sb := Sandbox{
		ID:                newID(),
		TenantID:          input.TenantID,
		NodeID:            nil,
		State:             SandboxRequested,
		VMMProfile:        input.VMMProfile,
		ImageRef:          input.ImageRef,
		CPUMillis:         input.CPUMillis,
		MemoryMiB:         input.MemoryMiB,
		StateVersion:      1,
		OwnerSub:          ownerSub,
		OwnerEmail:        ownerEmail,
		LastActivityAt:    now,
		WorkspaceHostPath: input.WorkspaceHostPath,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	ln, lnState := localNetFromInput(input)
	sb.LocalNet = ln
	sb.LocalNetState = lnState
	if sb.VMMProfile == "" {
		sb.VMMProfile = "cloud-hypervisor"
	}

	m.mu.Lock()
	nodeID := m.provisionNodeID
	if input.NodeID != "" {
		nodeID = input.NodeID
	}
	placed := !AutoProvisionEnabled()
	if placed {
		// Decide and insert under one lock: concurrent creates cannot overfill a node.
		usage := m.nodeUsageLocked()
		cands := make([]sched.Candidate, 0, len(m.nodes))
		for _, n := range m.nodes {
			cands = append(cands, Candidate(n, usage[n.ID]))
		}
		picked, err := sched.Place(m.schedCfg, placementRequest(input, sb.VMMProfile), cands, now)
		if err != nil {
			m.mu.Unlock()
			return Sandbox{}, err
		}
		nodeID = picked
		sb.NodeID = &picked
	}
	m.sandboxes[sb.ID] = sb
	out := cloneSandbox(sb)
	schedCfg := m.schedCfg
	m.mu.Unlock()

	_ = m.EmitEvent(EmitEventInput{
		SandboxID: out.ID,
		TenantID:  out.TenantID,
		EventType: "sandbox.created",
		ToState:   strPtr(string(SandboxRequested)),
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   json.RawMessage(`{}`),
	})
	if out.LocalNet {
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: out.ID,
			TenantID:  out.TenantID,
			EventType: "sandbox.local_net_requested",
			Actor:     "api",
			ActorSub:  actorSub,
			Payload:   mustJSON(map[string]any{"local_net": true, "local_net_state": out.LocalNetState}),
		})
	}
	if placed {
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: out.ID,
			TenantID:  out.TenantID,
			EventType: "sandbox.placed",
			Actor:     "scheduler",
			Payload:   placedEventPayload(nodeID, schedCfg, input),
		})
	}

	if AutoProvisionEnabled() {
		// Sync provisioner stub: requested → starting → running.
		return m.provisionStub(out.ID, nodeID)
	}
	return out, nil
}

func (m *MemoryStore) provisionStub(id, nodeID string) (Sandbox, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	from := string(sb.State)
	now := time.Now().UTC()
	sb.State = SandboxStarting
	sb.StateVersion++
	sb.UpdatedAt = now
	m.sandboxes[id] = sb
	tenantID := sb.TenantID
	m.mu.Unlock()

	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(SandboxStarting)),
		Actor:     "provisioner",
		Payload:   json.RawMessage(`{}`),
	})

	m.mu.Lock()
	sb = m.sandboxes[id]
	from2 := string(sb.State)
	nid := nodeID
	sb.NodeID = &nid
	sb.State = SandboxRunning
	sb.StateVersion++
	nowRun := time.Now().UTC()
	sb.UpdatedAt = nowRun
	sb.LastActivityAt = nowRun
	sb.StopReason = ""
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	m.mu.Unlock()

	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
		EventType: "sandbox.state_changed",
		FromState: &from2,
		ToState:   strPtr(string(SandboxRunning)),
		Actor:     "provisioner",
		Payload:   mustJSON(map[string]string{"node_id": nodeID}),
	})

	return out, nil
}

func (m *MemoryStore) GetSandbox(id string) (Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return Sandbox{}, ErrNotFound
	}
	return cloneSandbox(sb), nil
}

func (m *MemoryStore) ListSandboxes(tenantID string) ([]Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Sandbox, 0)
	for _, sb := range m.sandboxes {
		if tenantID != "" && sb.TenantID != tenantID {
			continue
		}
		out = append(out, cloneSandbox(sb))
	}
	return out, nil
}

func (m *MemoryStore) AssignSandbox(id, nodeID string, state SandboxState) (Sandbox, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	if nodeID == "" {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: node_id required", ErrInvalidInput)
	}
	from := string(sb.State)
	nid := nodeID
	sb.NodeID = &nid
	sb.State = state
	sb.StateVersion++
	sb.UpdatedAt = time.Now().UTC()
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	tenantID := sb.TenantID
	m.mu.Unlock()

	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(state)),
		Actor:     "api",
		Payload:   mustJSON(map[string]string{"node_id": nodeID}),
	})
	return out, nil
}

func (m *MemoryStore) ClaimSandbox(id, nodeID string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(nodeID) == "" {
		return Sandbox{}, fmt.Errorf("%w: id and node_id required", ErrInvalidInput)
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	// Only the node the scheduler placed the sandbox on can claim it (ADR-0011).
	if sb.NodeID == nil || *sb.NodeID != nodeID {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: sandbox is not assigned to node %s", ErrConflict, nodeID)
	}
	if sb.State != SandboxRequested {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: sandbox state %s not claimable", ErrConflict, sb.State)
	}
	now := time.Now().UTC()
	from := string(sb.State)
	until := leaseUntil(now)
	sb.State = SandboxStarting
	sb.NodeLeaseUntil = &until
	sb.StateVersion++
	sb.UpdatedAt = now
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	tenantID := sb.TenantID
	m.mu.Unlock()

	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
		EventType: "sandbox.claimed",
		FromState: &from,
		ToState:   strPtr(string(SandboxStarting)),
		Actor:     "node-agent",
		Payload:   mustJSON(map[string]string{"node_id": nodeID}),
	})
	return out, nil
}

func (m *MemoryStore) ListNodeWork(nodeID string) ([]Sandbox, error) {
	if strings.TrimSpace(nodeID) == "" {
		return nil, fmt.Errorf("%w: node_id required", ErrInvalidInput)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Sandbox, 0)
	for _, sb := range m.sandboxes {
		assigned := sb.NodeID != nil && *sb.NodeID == nodeID
		switch {
		case assigned && (sb.State == SandboxRequested || sb.State == SandboxStarting || sb.State == SandboxStopping):
			out = append(out, cloneSandbox(sb))
		case assigned && sb.LocalNet && sb.State == SandboxRunning:
			// Running full-tunnel sessions stay visible so the node can move
			// pending → up → withdrawn without ever restoring public egress.
			out = append(out, cloneSandbox(sb))
		}
	}
	return out, nil
}

func (m *MemoryStore) UpdateSandboxStatus(id string, state SandboxState, detail string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if !ValidAgentStatus(state) {
		return Sandbox{}, fmt.Errorf("%w: invalid status %s", ErrInvalidInput, state)
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	if !ValidAgentTransition(sb.State, state) {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: cannot move sandbox from %s to %s", ErrConflict, sb.State, state)
	}
	from := string(sb.State)
	now := time.Now().UTC()
	sb.State = state
	sb.StateVersion++
	sb.UpdatedAt = now
	if state == SandboxFailed || state == SandboxStopped || state == SandboxStopping {
		withdrawLocalNetFields(&sb)
	}
	if state == SandboxRunning || state == SandboxStarting {
		until := leaseUntil(now)
		sb.NodeLeaseUntil = &until
	}
	if state == SandboxRunning {
		sb.LastActivityAt = now
		sb.StopReason = ""
	}
	if state == SandboxStopped || state == SandboxFailed {
		sb.NodeLeaseUntil = nil
	}
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	tenantID := sb.TenantID
	m.mu.Unlock()

	payload := map[string]string{}
	if detail != "" {
		payload["detail"] = detail
	}
	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(state)),
		Actor:     "node-agent",
		Payload:   mustJSON(payload),
	})
	return out, nil
}

func (m *MemoryStore) RenewSandboxLease(id, nodeID string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(nodeID) == "" {
		return Sandbox{}, fmt.Errorf("%w: id and node_id required", ErrInvalidInput)
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	if sb.NodeID == nil || *sb.NodeID != nodeID {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: not owned by %s", ErrConflict, nodeID)
	}
	if !leaseRenewable(sb.State) {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: sandbox is %s, not active on %s", ErrConflict, sb.State, nodeID)
	}
	now := time.Now().UTC()
	until := leaseUntil(now)
	sb.NodeLeaseUntil = &until
	sb.UpdatedAt = now
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	m.mu.Unlock()
	return out, nil
}

func (m *MemoryStore) ReclaimExpiredLeases(now time.Time, reRequest bool) ([]Sandbox, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	type pending struct {
		id, tenant, from string
		to               SandboxState
	}
	var events []pending
	out := make([]Sandbox, 0)
	for id, sb := range m.sandboxes {
		if sb.State != SandboxStarting && sb.State != SandboxRunning {
			continue
		}
		if !leaseExpired(sb.NodeLeaseUntil, now) {
			continue
		}
		from := string(sb.State)
		if reRequest {
			sb.State = SandboxRequested
			sb.NodeID = nil
			sb.NodeLeaseUntil = nil
		} else {
			sb.State = SandboxFailed
			sb.NodeLeaseUntil = nil
		}
		sb.StateVersion++
		sb.UpdatedAt = now
		m.sandboxes[id] = sb
		out = append(out, cloneSandbox(sb))
		events = append(events, pending{id: id, tenant: sb.TenantID, from: from, to: sb.State})
	}
	m.mu.Unlock()
	for _, e := range events {
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: e.id,
			TenantID:  e.tenant,
			EventType: "sandbox.lease_expired",
			FromState: &e.from,
			ToState:   strPtr(string(e.to)),
			Actor:     "lease",
			Payload:   mustJSON(map[string]any{"re_request": reRequest}),
		})
	}
	return out, nil
}

func (m *MemoryStore) MarkSandboxStopping(id, actorSub string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	actorSub = strings.TrimSpace(actorSub)
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	if sb.State == SandboxStopped || sb.State == SandboxStopping {
		out := cloneSandbox(sb)
		m.mu.Unlock()
		return out, nil
	}
	// Never claimed → stopped immediately: no VM exists before a node claims it.
	if sb.State == SandboxRequested {
		from := string(sb.State)
		sb.State = SandboxStopped
		withdrawLocalNetFields(&sb)
		sb.StateVersion++
		sb.UpdatedAt = time.Now().UTC()
		m.sandboxes[id] = sb
		out := cloneSandbox(sb)
		tenantID := sb.TenantID
		m.mu.Unlock()
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: id,
			TenantID:  tenantID,
			EventType: "sandbox.state_changed",
			FromState: &from,
			ToState:   strPtr(string(SandboxStopped)),
			Actor:     "api",
			ActorSub:  actorSub,
			Payload:   json.RawMessage(`{"reason":"destroy_unclaimed"}`),
		})
		return out, nil
	}
	// failed: the node already tore the VM down. Destroy finishes as stopped
	// instead of 409, so the session file can be cleared.
	if sb.State == SandboxFailed {
		from := string(sb.State)
		sb.State = SandboxStopped
		withdrawLocalNetFields(&sb)
		sb.StateVersion++
		sb.UpdatedAt = time.Now().UTC()
		m.sandboxes[id] = sb
		out := cloneSandbox(sb)
		tenantID := sb.TenantID
		m.mu.Unlock()
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: id,
			TenantID:  tenantID,
			EventType: "sandbox.state_changed",
			FromState: &from,
			ToState:   strPtr(string(SandboxStopped)),
			Actor:     "api",
			ActorSub:  actorSub,
			Payload:   json.RawMessage(`{"reason":"destroy_failed"}`),
		})
		return out, nil
	}
	if !IsActiveLifecycle(sb.State) {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: cannot destroy from state %s", ErrConflict, sb.State)
	}
	from := string(sb.State)
	sb.State = SandboxStopping
	withdrawLocalNetFields(&sb)
	sb.StateVersion++
	sb.UpdatedAt = time.Now().UTC()
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	tenantID := sb.TenantID
	m.mu.Unlock()

	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenantID,
		EventType: "sandbox.state_changed",
		FromState: &from,
		ToState:   strPtr(string(SandboxStopping)),
		Actor:     "api",
		ActorSub:  actorSub,
		Payload:   json.RawMessage(`{"reason":"destroy"}`),
	})
	return out, nil
}

func (m *MemoryStore) TouchSandboxActivity(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return ErrNotFound
	}
	sb.LastActivityAt = now
	m.sandboxes[id] = sb
	return nil
}

func (m *MemoryStore) StopIdleSandboxes(now time.Time, idleFor time.Duration) ([]Sandbox, error) {
	if idleFor <= 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	type pending struct {
		id, tenant, from string
		to               SandboxState
	}
	var events []pending
	out := make([]Sandbox, 0)
	for id, sb := range m.sandboxes {
		if !IsActiveLifecycle(sb.State) {
			continue
		}
		if DecideIdle(sb.LastActivityAt, now, idleFor) != IdleExpired {
			continue
		}
		from := string(sb.State)
		target := SandboxStopping
		if sb.State == SandboxRequested { // never claimed: no VM to stop
			target = SandboxStopped
		}
		sb.State = target
		withdrawLocalNetFields(&sb)
		sb.StopReason = StopReasonIdle
		sb.NodeLeaseUntil = nil
		sb.StateVersion++
		sb.UpdatedAt = now
		m.sandboxes[id] = sb
		out = append(out, cloneSandbox(sb))
		events = append(events, pending{id: id, tenant: sb.TenantID, from: from, to: target})
	}
	m.mu.Unlock()
	for _, e := range events {
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: e.id,
			TenantID:  e.tenant,
			EventType: "sandbox.idle_reaped",
			FromState: &e.from,
			ToState:   strPtr(string(e.to)),
			Actor:     "idle-reaper",
			Payload:   mustJSON(map[string]any{"reason": StopReasonIdle, "idle_for": idleFor.String()}),
		})
	}
	return out, nil
}

// SetLastActivityForTest pins last_activity_at (unit tests only).
func (m *MemoryStore) SetLastActivityForTest(id string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return
	}
	sb.LastActivityAt = at.UTC()
	m.sandboxes[id] = sb
}

func (m *MemoryStore) RegisterNode(input RegisterNodeInput) (Node, error) {
	if strings.TrimSpace(input.ID) == "" && strings.TrimSpace(input.Name) == "" {
		return Node{}, fmt.Errorf("%w: id or name required", ErrInvalidInput)
	}
	if err := validateNodeCapacity(input.CapacityCPU, input.CapacityMemMiB, input.MaxSandboxes); err != nil {
		return Node{}, err
	}
	now := time.Now().UTC()
	id := input.ID
	if id == "" {
		id = newID()
	}
	name := input.Name
	if name == "" {
		name = id
	}
	profiles := input.VMMProfiles
	if len(profiles) == 0 {
		profiles = []string{"cloud-hypervisor"}
	}
	seen := now
	agentEndpoint := input.AgentEndpoint
	if agentEndpoint == "" {
		agentEndpoint = input.Endpoint
	}
	node := Node{
		ID:             id,
		Name:           name,
		Endpoint:       input.Endpoint,
		AgentEndpoint:  agentEndpoint,
		State:          "ready",
		VMMProfiles:    append([]string(nil), profiles...),
		CapacityCPU:    input.CapacityCPU,
		CapacityMemMiB: input.CapacityMemMiB,
		MaxSandboxes:   input.MaxSandboxes,
		AcceptsWork:    input.acceptsWork(),
		LocalNetDial:   strings.TrimSpace(input.LocalNetDial),
		FenceEndpoint:  strings.TrimSpace(input.FenceEndpoint),
		FenceToken:     strings.TrimSpace(input.FenceToken),
		LastSeenAt:     &seen,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.nodes[id]; ok {
		if existing.RevokedAt != nil {
			return Node{}, errNodeRevoked(id)
		}
		node.CreatedAt = existing.CreatedAt
		node.CertFingerprint = existing.CertFingerprint
		node.CertSerial = existing.CertSerial
		node.EnrolledAt = existing.EnrolledAt
		node.RevokedAt = existing.RevokedAt
		// Cordon is an admin decision; an agent re-registering never lifts it.
		node.Cordoned = existing.Cordoned
		if node.AgentEndpoint == "" {
			node.AgentEndpoint = existing.AgentEndpoint
		}
		if node.FenceEndpoint == "" {
			node.FenceEndpoint = existing.FenceEndpoint
		}
		if node.FenceToken == "" {
			node.FenceToken = existing.FenceToken
		}
		m.nodes[id] = node
		return cloneNode(node), nil
	}
	m.nodes[id] = node
	return cloneNode(node), nil
}

func (m *MemoryStore) EnrollNode(input EnrollNodeInput, cert CertMeta) (Node, error) {
	fp := strings.TrimSpace(cert.Fingerprint)
	if fp == "" {
		return Node{}, fmt.Errorf("%w: cert_fingerprint required", ErrInvalidInput)
	}
	if strings.TrimSpace(input.ID) == "" && strings.TrimSpace(input.Name) == "" {
		return Node{}, fmt.Errorf("%w: id or name required", ErrInvalidInput)
	}
	now := time.Now().UTC()
	id := input.ID
	if id == "" {
		id = newID()
	}
	name := input.Name
	if name == "" {
		name = id
	}
	profiles := input.VMMProfiles
	if len(profiles) == 0 {
		profiles = []string{"cloud-hypervisor"}
	}
	agentEndpoint := input.AgentEndpoint
	if agentEndpoint == "" {
		agentEndpoint = input.Endpoint
	}
	enrolled := now
	seen := now
	node := Node{
		ID:              id,
		Name:            name,
		Endpoint:        input.Endpoint,
		AgentEndpoint:   agentEndpoint,
		State:           "ready",
		VMMProfiles:     append([]string(nil), profiles...),
		CapacityCPU:     input.CapacityCPU,
		CapacityMemMiB:  input.CapacityMemMiB,
		AcceptsWork:     true,
		CertFingerprint: fp,
		CertSerial:      strings.TrimSpace(cert.Serial),
		EnrolledAt:      &enrolled,
		LastSeenAt:      &seen,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.nodes[id]; ok {
		node.CreatedAt = existing.CreatedAt
		// Same as Postgres: enroll does not carry fence or scheduling settings, so keep
		// the registered ones (and an admin's cordon).
		node.FenceEndpoint = existing.FenceEndpoint
		node.FenceToken = existing.FenceToken
		node.MaxSandboxes = existing.MaxSandboxes
		node.Cordoned = existing.Cordoned
		node.AcceptsWork = existing.AcceptsWork
		node.LocalNetDial = existing.LocalNetDial
		// Re-enroll clears prior revoke so a fresh cert can talk again after rotate/re-enroll.
		if existing.CertFingerprint != "" && existing.CertFingerprint != fp {
			m.revokedCerts[existing.CertFingerprint] = id
		}
	}
	delete(m.revokedCerts, fp)
	m.nodes[id] = node
	return cloneNode(node), nil
}

func (m *MemoryStore) RotateNodeCert(nodeID string, cert CertMeta) (Node, error) {
	fp := strings.TrimSpace(cert.Fingerprint)
	if strings.TrimSpace(nodeID) == "" {
		return Node{}, fmt.Errorf("%w: node id required", ErrInvalidInput)
	}
	if fp == "" {
		return Node{}, fmt.Errorf("%w: cert_fingerprint required", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[nodeID]
	if !ok {
		return Node{}, ErrNotFound
	}
	if n.RevokedAt != nil {
		return Node{}, fmt.Errorf("%w: node is revoked; re-enroll required", ErrConflict)
	}
	now := time.Now().UTC()
	if n.CertFingerprint != "" && n.CertFingerprint != fp {
		m.revokedCerts[n.CertFingerprint] = nodeID
	}
	n.CertFingerprint = fp
	n.CertSerial = strings.TrimSpace(cert.Serial)
	n.UpdatedAt = now
	delete(m.revokedCerts, fp)
	m.nodes[nodeID] = n
	return cloneNode(n), nil
}

func (m *MemoryStore) RevokeNode(nodeID string) (Node, error) {
	if strings.TrimSpace(nodeID) == "" {
		return Node{}, fmt.Errorf("%w: node id required", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[nodeID]
	if !ok {
		return Node{}, ErrNotFound
	}
	now := time.Now().UTC()
	n.RevokedAt = &now
	n.State = "offline"
	n.UpdatedAt = now
	if n.CertFingerprint != "" {
		m.revokedCerts[n.CertFingerprint] = nodeID
	}
	m.nodes[nodeID] = n
	return cloneNode(n), nil
}

func (m *MemoryStore) IsCertRevoked(fingerprint string) (bool, error) {
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return false, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.revokedCerts[fp]; ok {
		return true, nil
	}
	for _, n := range m.nodes {
		if n.CertFingerprint == fp && n.RevokedAt != nil {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemoryStore) HeartbeatNode(id string) (Node, error) {
	if strings.TrimSpace(id) == "" {
		return Node{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	if n.RevokedAt != nil {
		return Node{}, errNodeRevoked(id)
	}
	now := time.Now().UTC()
	n.LastSeenAt = &now
	n.UpdatedAt = now
	n.State = "ready"
	m.nodes[id] = n
	return cloneNode(n), nil
}

// nodePollWriteEvery throttles last_seen_at writes from work polls (every ~2s per node).
const nodePollWriteEvery = 5 * time.Second

func (m *MemoryStore) TouchNodePoll(id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return ErrNotFound
	}
	if n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) < nodePollWriteEvery {
		return nil
	}
	n.LastSeenAt = &now
	n.UpdatedAt = now
	if n.RevokedAt == nil {
		n.State = "ready"
	}
	m.nodes[id] = n
	return nil
}

func (m *MemoryStore) SetNodeCordoned(id string, cordoned bool) (Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	n.Cordoned = cordoned
	n.UpdatedAt = time.Now().UTC()
	m.nodes[id] = n
	return cloneNode(n), nil
}

func (m *MemoryStore) ListNodeUsage() (map[string]NodeUsage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nodeUsageLocked(), nil
}

func (m *MemoryStore) ListNodes() ([]Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, cloneNode(n))
	}
	return out, nil
}

func (m *MemoryStore) GetNode(id string) (Node, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[id]
	if !ok {
		return Node{}, ErrNotFound
	}
	return cloneNode(n), nil
}

func (m *MemoryStore) EmitEvent(input EmitEventInput) error {
	if strings.TrimSpace(input.SandboxID) == "" || strings.TrimSpace(input.EventType) == "" {
		return fmt.Errorf("%w: sandbox_id and event_type required", ErrInvalidInput)
	}
	actor := input.Actor
	if actor == "" {
		actor = "system"
	}
	payload := input.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextEvt++
	m.events = append(m.events, SandboxEvent{
		ID:        m.nextEvt,
		SandboxID: input.SandboxID,
		TenantID:  input.TenantID,
		EventType: input.EventType,
		FromState: cloneStr(input.FromState),
		ToState:   cloneStr(input.ToState),
		Actor:     actor,
		ActorSub:  strings.TrimSpace(input.ActorSub),
		RequestID: cloneStr(input.RequestID),
		Payload:   append(json.RawMessage(nil), payload...),
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

func (m *MemoryStore) ListEvents(sandboxID string) ([]SandboxEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SandboxEvent, 0)
	for _, e := range m.events {
		if e.SandboxID == sandboxID {
			cp := e
			cp.Payload = append(json.RawMessage(nil), e.Payload...)
			cp.FromState = cloneStr(e.FromState)
			cp.ToState = cloneStr(e.ToState)
			cp.RequestID = cloneStr(e.RequestID)
			out = append(out, cp)
		}
	}
	return out, nil
}

func (m *MemoryStore) LookupAPIKeyByHash(secretHash string) (ApiKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.apiKeys[secretHash]
	if !ok || k.RevokedAt != nil {
		return ApiKey{}, ErrNotFound
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now().UTC()) {
		return ApiKey{}, ErrNotFound
	}
	return k, nil
}

func (m *MemoryStore) EnsureAPIKey(tenantID, name, keyPrefix, secretHash string) (ApiKey, error) {
	if tenantID == "" || name == "" || keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: tenant_id, name, key_prefix, secret_hash required", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, k := range m.apiKeys {
		if k.TenantID == tenantID && k.Name == name {
			if k.SecretHash != secretHash {
				delete(m.apiKeys, hash)
				k.SecretHash = secretHash
				k.KeyPrefix = keyPrefix
				m.apiKeys[secretHash] = k
			}
			return k, nil
		}
	}
	k := ApiKey{
		ID:         newID(),
		TenantID:   tenantID,
		Name:       name,
		KeyPrefix:  keyPrefix,
		SecretHash: secretHash,
		CreatedAt:  time.Now().UTC(),
	}
	m.apiKeys[secretHash] = k
	return k, nil
}

func (m *MemoryStore) CountAPIKeys() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var n int64
	for _, k := range m.apiKeys {
		if k.RevokedAt == nil {
			n++
		}
	}
	return n, nil
}

func (m *MemoryStore) TouchAPIKey(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for hash, k := range m.apiKeys {
		if k.ID == id {
			k.LastUsedAt = &now
			m.apiKeys[hash] = k
			return nil
		}
	}
	return ErrNotFound
}

func prepareCreateSandbox(input *CreateSandboxInput) error {
	ws, err := cleanWorkspaceHostPath(input.WorkspaceHostPath)
	if err != nil {
		return err
	}
	input.WorkspaceHostPath = ws
	return validateCreateSandbox(*input)
}

func cleanWorkspaceHostPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.ContainsRune(p, '\x00') {
		return "", fmt.Errorf("%w: workspace_host_path contains NUL", ErrInvalidInput)
	}
	if len(p) > 4096 {
		return "", fmt.Errorf("%w: workspace_host_path too long", ErrInvalidInput)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: workspace_host_path must be absolute", ErrInvalidInput)
	}
	return filepath.Clean(p), nil
}

func validateCreateSandbox(input CreateSandboxInput) error {
	if strings.TrimSpace(input.TenantID) == "" {
		return fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	if strings.TrimSpace(input.ImageRef) == "" {
		return fmt.Errorf("%w: image_ref required", ErrInvalidInput)
	}
	if input.CPUMillis <= 0 {
		return fmt.Errorf("%w: cpu_millis must be > 0", ErrInvalidInput)
	}
	if input.MemoryMiB <= 0 {
		return fmt.Errorf("%w: memory_mib must be > 0", ErrInvalidInput)
	}
	return nil
}

func cloneSandbox(sb Sandbox) Sandbox {
	if sb.NodeID != nil {
		nid := *sb.NodeID
		sb.NodeID = &nid
	}
	if sb.NodeLeaseUntil != nil {
		t := *sb.NodeLeaseUntil
		sb.NodeLeaseUntil = &t
	}
	return sb
}

func cloneNode(n Node) Node {
	n.VMMProfiles = append([]string(nil), n.VMMProfiles...)
	if n.LastSeenAt != nil {
		t := *n.LastSeenAt
		n.LastSeenAt = &t
	}
	if n.EnrolledAt != nil {
		t := *n.EnrolledAt
		n.EnrolledAt = &t
	}
	if n.RevokedAt != nil {
		t := *n.RevokedAt
		n.RevokedAt = &t
	}
	return n
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func strPtr(s string) *string { return &s }

func cloneStr(p *string) *string {
	if p == nil {
		return nil
	}
	s := *p
	return &s
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// ExpireLeaseForTest sets node_lease_until in the past (unit tests only).
func (m *MemoryStore) ExpireLeaseForTest(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return
	}
	past := time.Now().UTC().Add(-time.Minute)
	sb.NodeLeaseUntil = &past
	m.sandboxes[id] = sb
}

func (m *MemoryStore) PutAttestation(input PutAttestationInput) (AttestationRecord, error) {
	if strings.TrimSpace(input.SandboxID) == "" {
		return AttestationRecord{}, fmt.Errorf("%w: sandbox_id required", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[input.SandboxID]; !ok {
		return AttestationRecord{}, ErrNotFound
	}
	now := time.Now().UTC()
	rec := AttestationRecord{
		SandboxID:   input.SandboxID,
		NodeID:      input.NodeID,
		ImageDigest: input.ImageDigest,
		VMMProfile:  input.VMMProfile,
		CID:         input.CID,
		StatementTS: input.StatementTS,
		Alg:         input.Alg,
		KeyID:       input.KeyID,
		Signature:   input.Signature,
		Bundle:      input.Bundle,
		ReceivedAt:  now,
	}
	if len(rec.Bundle) == 0 {
		rec.Bundle = json.RawMessage(`{}`)
	}
	m.attestations[input.SandboxID] = rec
	return rec, nil
}

func (m *MemoryStore) GetAttestation(sandboxID string) (AttestationRecord, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return AttestationRecord{}, fmt.Errorf("%w: sandbox_id required", ErrInvalidInput)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.attestations[sandboxID]
	if !ok {
		return AttestationRecord{}, ErrNotFound
	}
	out := rec
	if len(out.Bundle) > 0 {
		out.Bundle = append(json.RawMessage(nil), out.Bundle...)
	}
	return out, nil
}

func (m *MemoryStore) ListEgressRules(tenantID string) ([]EgressRule, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	rules := m.egress[tenantID]
	out := make([]EgressRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, cloneEgressRule(r))
	}
	return out, nil
}

func (m *MemoryStore) PutEgressRules(tenantID string, rules []EgressRule) ([]EgressRule, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	cleaned := make([]EgressRule, 0, len(rules))
	for _, r := range rules {
		hp := strings.TrimSpace(strings.ToLower(r.HostPattern))
		if hp == "" {
			return nil, fmt.Errorf("%w: host_pattern required", ErrInvalidInput)
		}
		if r.Port != nil && (*r.Port <= 0 || *r.Port > 65535) {
			return nil, fmt.Errorf("%w: port out of range", ErrInvalidInput)
		}
		nr := EgressRule{
			ID:          r.ID,
			TenantID:    tenantID,
			HostPattern: hp,
			Port:        cloneInt(r.Port),
			Enabled:     r.Enabled,
		}
		if nr.ID == "" {
			nr.ID = newID()
		}
		// Default enabled=true when omitted in JSON is handled by caller; here trust Enabled field.
		cleaned = append(cleaned, nr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.egress[tenantID] = cleaned
	out := make([]EgressRule, 0, len(cleaned))
	for _, r := range cleaned {
		out = append(out, cloneEgressRule(r))
	}
	return out, nil
}

func cloneEgressRule(r EgressRule) EgressRule {
	r.Port = cloneInt(r.Port)
	return r
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

var _ Store = (*MemoryStore)(nil)

// IssueLocalNetGrant mints a one-shot grant for the local agent. The clear
// value is returned and not stored. State stays pending until HeartbeatLocalNet.
func (m *MemoryStore) IssueLocalNetGrant(id, dial string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	if strings.TrimSpace(id) == "" {
		return "", time.Time{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if ttl <= 0 {
		ttl = LocalNetGrantTTL
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	clear, hash, err := newLocalNetGrant()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := now.Add(ttl)
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return "", time.Time{}, ErrNotFound
	}
	if !sb.LocalNet {
		return "", time.Time{}, fmt.Errorf("%w: local_net is off; attach does not enable it", ErrConflict)
	}
	if localNetTerminal(sb.State) {
		return "", time.Time{}, fmt.Errorf("%w: sandbox not active", ErrConflict)
	}
	_ = dial
	sb.LocalNetGrantHash = hash
	sb.LocalNetGrantExpiresAt = &exp
	sb.UpdatedAt = now
	m.sandboxes[id] = sb
	return clear, exp, nil
}

// HeartbeatLocalNet marks the tunnel up when the grant matches. It does not
// move last_activity_at. An expired grant withdraws to blackhole (not public egress).
func (m *MemoryStore) HeartbeatLocalNet(id, grant, clientPub string, now time.Time) (Sandbox, error) {
	grant = strings.TrimSpace(grant)
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if grant == "" {
		return Sandbox{}, fmt.Errorf("%w: grant required", ErrInvalidInput)
	}
	if err := ValidateWGPublicKey(clientPub); err != nil {
		return Sandbox{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	activity := sb.LastActivityAt
	if !sb.LocalNet {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: local_net is off", ErrConflict)
	}
	if localNetTerminal(sb.State) {
		m.mu.Unlock()
		return Sandbox{}, fmt.Errorf("%w: sandbox not active", ErrConflict)
	}
	if sb.LocalNetGrantHash == "" || sb.LocalNetGrantExpiresAt == nil || !now.Before(*sb.LocalNetGrantExpiresAt) {
		withdrawLocalNetFields(&sb)
		sb.LastActivityAt = activity
		sb.UpdatedAt = now
		m.sandboxes[id] = sb
		tenant := sb.TenantID
		m.mu.Unlock()
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: id,
			TenantID:  tenant,
			EventType: "sandbox.local_net_withdrawn",
			Actor:     "local-agent",
			Payload:   mustJSON(map[string]any{"reason": "grant_expired", "public_egress": false}),
		})
		return Sandbox{}, fmt.Errorf("%w: local_net grant expired", ErrUnauthorized)
	}
	if hashLocalNetGrant(grant) != sb.LocalNetGrantHash {
		m.mu.Unlock()
		return Sandbox{}, ErrUnauthorized
	}
	sb.LocalNetState = LocalNetUp
	sb.LocalNetClientPublic = strings.TrimSpace(clientPub)
	attached := now
	sb.LocalNetAttachedAt = &attached
	sb.LastActivityAt = activity
	sb.UpdatedAt = now
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	tenant := sb.TenantID
	m.mu.Unlock()
	_ = m.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  tenant,
		EventType: "sandbox.local_net_up",
		Actor:     "local-agent",
		Payload:   mustJSON(map[string]any{"local_net_state": LocalNetUp, "public_egress": false}),
	})
	return out, nil
}

// WithdrawLocalNet drops the tunnel. local_net stays true so egress stays
// blackholed instead of returning to the node proxy.
func (m *MemoryStore) WithdrawLocalNet(id string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return Sandbox{}, ErrNotFound
	}
	prev := sb.LocalNetState
	activity := sb.LastActivityAt
	if sb.LocalNet {
		withdrawLocalNetFields(&sb)
	}
	sb.LastActivityAt = activity
	sb.UpdatedAt = time.Now().UTC()
	m.sandboxes[id] = sb
	out := cloneSandbox(sb)
	tenant := sb.TenantID
	m.mu.Unlock()
	if sb.LocalNet && prev != LocalNetWithdrawn {
		_ = m.EmitEvent(EmitEventInput{
			SandboxID: id,
			TenantID:  tenant,
			EventType: "sandbox.local_net_withdrawn",
			Actor:     "api",
			Payload:   mustJSON(map[string]any{"reason": "detach", "public_egress": false}),
		})
	}
	return out, nil
}

// SetLocalNetNodePublic stores the node device public key. The private key
// stays on the node. This does not move local_net_state or last_activity_at.
func (m *MemoryStore) SetLocalNetNodePublic(id, publicKey string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if err := ValidateWGPublicKey(publicKey); err != nil {
		return Sandbox{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sb, ok := m.sandboxes[id]
	if !ok {
		return Sandbox{}, ErrNotFound
	}
	if !sb.LocalNet {
		return Sandbox{}, fmt.Errorf("%w: local_net is off", ErrConflict)
	}
	sb.LocalNetNodePublic = strings.TrimSpace(publicKey)
	sb.UpdatedAt = time.Now().UTC()
	m.sandboxes[id] = sb
	return cloneSandbox(sb), nil
}
