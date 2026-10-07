package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The node half of SQLiteStore: registration, enrollment and certificates, liveness.

func getNodeSQL(ctx context.Context, q runner, id string) (Node, error) {
	n, err := scanNodeSQL(rowOf(ctx, q, `SELECT `+nodeColumns+` FROM nodes WHERE id=?`, id))
	if isNoRows(err) {
		return Node{}, ErrNotFound
	}
	return n, err
}

// saveNodeSQL writes every column a registration, a heartbeat or an admin can change.
func saveNodeSQL(ctx context.Context, q runner, n Node) error {
	res, err := run(ctx, q, `
		UPDATE nodes SET
			name=?, endpoint=?, agent_endpoint=?, state=?, vmm_profiles=?,
			capacity_cpu=?, capacity_mem_mib=?, max_sandboxes=?, cordoned=?, accepts_work=?, local_net_dial=?,
			agent_instance_id=?, agent_version=?, guest_kernel_digest=?, guest_image_digest=?, egress_enforced=?,
			cert_fingerprint=?, cert_serial=?, cert_not_after=?, fence_token=?, fence_endpoint=?,
			enrolled_at=?, revoked_at=?, last_seen_at=?, disk_free_mib=?, updated_at=?
		WHERE id=?`,
		n.Name, n.Endpoint, n.AgentEndpoint, n.State, n.VMMProfiles,
		n.CapacityCPU, n.CapacityMemMiB, n.MaxSandboxes, n.Cordoned, n.AcceptsWork, n.LocalNetDial,
		n.AgentInstanceID, n.AgentVersion, n.GuestKernelDigest, n.GuestImageDigest, n.EgressEnforced,
		n.CertFingerprint, n.CertSerial, n.CertNotAfter, n.FenceToken, n.FenceEndpoint,
		n.EnrolledAt, n.RevokedAt, n.LastSeenAt, n.DiskFreeMiB, n.UpdatedAt,
		n.ID)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

func insertNodeSQL(ctx context.Context, q runner, n Node) error {
	_, err := run(ctx, q, `
		INSERT INTO nodes (
			id, name, endpoint, agent_endpoint, state, vmm_profiles,
			capacity_cpu, capacity_mem_mib, max_sandboxes, cordoned, accepts_work, local_net_dial,
			agent_instance_id, agent_version, guest_kernel_digest, guest_image_digest, egress_enforced,
			cert_fingerprint, cert_serial, cert_not_after, fence_token, fence_endpoint,
			enrolled_at, revoked_at, last_seen_at, disk_free_mib, created_at, updated_at
		) VALUES (?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?, ?,?,?,?,?,?)`,
		n.ID, n.Name, n.Endpoint, n.AgentEndpoint, n.State, n.VMMProfiles,
		n.CapacityCPU, n.CapacityMemMiB, n.MaxSandboxes, n.Cordoned, n.AcceptsWork, n.LocalNetDial,
		n.AgentInstanceID, n.AgentVersion, n.GuestKernelDigest, n.GuestImageDigest, n.EgressEnforced,
		n.CertFingerprint, n.CertSerial, n.CertNotAfter, n.FenceToken, n.FenceEndpoint,
		n.EnrolledAt, n.RevokedAt, n.LastSeenAt, n.DiskFreeMiB, n.CreatedAt, n.UpdatedAt)
	return err
}

func (s *SQLiteStore) RegisterNode(ctx context.Context, input RegisterNodeInput) (Node, error) {
	if strings.TrimSpace(input.ID) == "" && strings.TrimSpace(input.Name) == "" {
		return Node{}, fmt.Errorf("%w: id or name required", ErrInvalidInput)
	}
	if err := validateNodeCapacity(input.CapacityCPU, input.CapacityMemMiB, input.MaxSandboxes); err != nil {
		return Node{}, err
	}
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
	now := time.Now().UTC()
	seen := now
	node := Node{
		ID: id, Name: name, Endpoint: input.Endpoint, AgentEndpoint: agentEndpoint, State: "ready",
		VMMProfiles: append([]string(nil), profiles...), CapacityCPU: input.CapacityCPU,
		CapacityMemMiB: input.CapacityMemMiB, MaxSandboxes: input.MaxSandboxes, AcceptsWork: input.acceptsWork(),
		LocalNetDial: strings.TrimSpace(input.LocalNetDial), EgressEnforced: input.EgressEnforced,
		AgentInstanceID: strings.TrimSpace(input.AgentInstanceID), AgentVersion: strings.TrimSpace(input.AgentVersion),
		GuestKernelDigest: strings.TrimSpace(input.GuestKernelDigest), GuestImageDigest: strings.TrimSpace(input.GuestImageDigest),
		LastSeenAt: &seen, CreatedAt: now, UpdatedAt: now,
	}

	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		existing, err := getNodeSQL(ctx, tx, id)
		isNew := errors.Is(err, ErrNotFound)
		if err != nil && !isNew {
			return err
		}
		eventType := "node.registered"
		if isNew {
			if err := insertNodeSQL(ctx, tx, node); err != nil {
				return sqliteNodeWriteError(err, name)
			}
		} else {
			if existing.RevokedAt != nil {
				return errNodeRevoked(id)
			}
			eventType = "node.updated"
			if node.AgentInstanceID == "" {
				node.AgentInstanceID = existing.AgentInstanceID
			}
			node.CreatedAt = existing.CreatedAt
			node.CertFingerprint = existing.CertFingerprint
			node.CertSerial = existing.CertSerial
			node.CertNotAfter = existing.CertNotAfter
			node.EnrolledAt = existing.EnrolledAt
			node.RevokedAt = existing.RevokedAt
			// Cordon is an admin decision; an agent re-registering never lifts it.
			node.Cordoned = existing.Cordoned
			node.DiskFreeMiB = existing.DiskFreeMiB
			// The fence target is an admin decision too (SetNodeFence).
			node.FenceEndpoint = existing.FenceEndpoint
			node.FenceToken = existing.FenceToken
			if node.AgentEndpoint == "" {
				node.AgentEndpoint = existing.AgentEndpoint
			}
			if err := saveNodeSQL(ctx, tx, node); err != nil {
				return sqliteNodeWriteError(err, name)
			}
			if agentRestarted(existing.AgentInstanceID, node.AgentInstanceID) {
				if err := failRestartOrphansSQL(ctx, tx, id, now, input.AdoptedSandboxes); err != nil {
					return err
				}
			}
		}
		if err := nodeEventSQL(ctx, tx, id, eventType, "node-agent", map[string]any{
			"endpoint": input.Endpoint, "agent_endpoint": agentEndpoint, "name": name,
		}); err != nil {
			return fmt.Errorf("node event: %w", err)
		}
		out, err = getNodeSQL(ctx, tx, id)
		return err
	})
	return out, err
}

// failRestartOrphansSQL fails, inside the register transaction, the sandboxes a restarted
// agent lost track of (see restartOrphanTarget): not those it says it adopted.
func failRestartOrphansSQL(ctx context.Context, tx *sql.Tx, nodeID string, now time.Time, adopted []string) error {
	kept := map[string]bool{}
	for _, id := range adopted {
		kept[id] = true
	}
	rows, err := rowsOf(ctx, tx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE node_id=?`, nodeID)
	if err != nil {
		return err
	}
	all, err := collectSandboxes(rows)
	if err != nil {
		return err
	}
	for _, sb := range all {
		if kept[sb.ID] {
			continue
		}
		to, ok := restartOrphanTarget(sb.State)
		if !ok {
			continue
		}
		from := string(sb.State)
		sb.State = to
		if to == SandboxStopped {
			sb.StoppedAt = &now
		}
		withdrawLocalNetFields(&sb)
		sb.StopReason = StopReasonAgentRestarted
		sb.StateVersion++
		sb.UpdatedAt = now
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		if err := emitEventSQL(ctx, tx, EmitEventInput{
			SandboxID: sb.ID, TenantID: sb.TenantID, EventType: "sandbox.agent_restarted",
			FromState: &from, ToState: strPtr(string(to)), Actor: "node-agent",
			Payload: mustJSON(map[string]string{"node_id": nodeID, "reason": StopReasonAgentRestarted}),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) CreateEnrollToken(ctx context.Context, tok EnrollToken) error {
	if err := validateEnrollToken(tok); err != nil {
		return err
	}
	now := time.Now().UTC()
	if tok.CreatedAt.IsZero() {
		tok.CreatedAt = now
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, _ = run(ctx, tx, `DELETE FROM node_enroll_tokens WHERE expires_at < ?`, now.Add(-enrollTokenRetention))
		res, err := run(ctx, tx, `
			INSERT INTO node_enroll_tokens (hash, node_id, expires_at, created_by, created_at)
			VALUES (?, NULLIF(?, ''), ?, ?, ?)
			ON CONFLICT (hash) DO NOTHING`,
			tok.Hash, strings.TrimSpace(tok.NodeID), tok.ExpiresAt, tok.CreatedBy, tok.CreatedAt)
		if err != nil {
			return err
		}
		if affected(res) == 0 {
			return ErrAlreadyExists
		}
		return nil
	})
}

func (s *SQLiteStore) CheckEnroll(ctx context.Context, id string, auth EnrollAuth) error {
	_, _, err := enrollCheckSQL(ctx, s.db, id, auth, time.Now().UTC())
	return err
}

// enrollCheckSQL reads the enroll token and the node's certificate state and applies
// enrollAllowed. It returns the node's current certificate fingerprint ("" if none) and
// whether the node exists.
func enrollCheckSQL(ctx context.Context, q runner, id string, auth EnrollAuth, now time.Time) (oldFP string, exists bool, err error) {
	var tok *EnrollToken
	if auth.TokenHash != "" {
		var t EnrollToken
		var pinned *string
		err := rowOf(ctx, q, `SELECT node_id, expires_at, used_at FROM node_enroll_tokens WHERE hash=?`, auth.TokenHash).
			Scan(&pinned, tsCol{&t.ExpiresAt}, tsPtrCol{&t.UsedAt})
		if isNoRows(err) {
			return "", false, ErrEnrollTokenInvalid
		}
		if err != nil {
			return "", false, err
		}
		if pinned != nil {
			t.NodeID = *pinned
		}
		tok = &t
	}
	var revokedAt *time.Time
	err = rowOf(ctx, q, `SELECT cert_fingerprint, revoked_at FROM nodes WHERE id=?`, id).Scan(&oldFP, tsPtrCol{&revokedAt})
	switch {
	case isNoRows(err):
		oldFP, exists = "", false
	case err != nil:
		return "", false, err
	default:
		exists = true
	}
	live := exists && enrolledLive(Node{CertFingerprint: oldFP, RevokedAt: revokedAt})
	return oldFP, exists, enrollAllowed(id, tok, live, now)
}

func (s *SQLiteStore) EnrollNode(ctx context.Context, input EnrollNodeInput, cert CertMeta, auth EnrollAuth) (Node, error) {
	fp := strings.TrimSpace(cert.Fingerprint)
	serial := strings.TrimSpace(cert.Serial)
	if fp == "" {
		return Node{}, fmt.Errorf("%w: cert_fingerprint required", ErrInvalidInput)
	}
	if strings.TrimSpace(input.ID) == "" && strings.TrimSpace(input.Name) == "" {
		return Node{}, fmt.Errorf("%w: id or name required", ErrInvalidInput)
	}
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
	now := time.Now().UTC()

	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		oldFP, exists, err := enrollCheckSQL(ctx, tx, id, auth, now)
		if err != nil {
			return err
		}
		if auth.TokenHash != "" {
			if _, err := run(ctx, tx, `UPDATE node_enroll_tokens SET used_at=?, used_by=? WHERE hash=?`, now, id, auth.TokenHash); err != nil {
				return err
			}
		}
		if exists {
			if oldFP != "" && oldFP != fp {
				_, _ = run(ctx, tx, `
					INSERT INTO node_cert_revocations (fingerprint, node_id, serial, reason, revoked_at)
					VALUES (?,?,'','rotated-on-enroll',?)
					ON CONFLICT (fingerprint) DO NOTHING`, oldFP, id, now)
			}
			// Enroll does not carry fence or scheduling settings: the registered ones stay
			// (and an admin's cordon).
			n, err := getNodeSQL(ctx, tx, id)
			if err != nil {
				return err
			}
			enrolled, seen := now, now
			n.Name, n.Endpoint, n.AgentEndpoint, n.State = name, input.Endpoint, agentEndpoint, "ready"
			n.VMMProfiles = append([]string(nil), profiles...)
			n.CapacityCPU, n.CapacityMemMiB = input.CapacityCPU, input.CapacityMemMiB
			n.CertFingerprint, n.CertSerial, n.CertNotAfter = fp, serial, cert.notAfterPtr()
			n.EnrolledAt, n.LastSeenAt, n.UpdatedAt, n.RevokedAt = &enrolled, &seen, now, nil
			if err := saveNodeSQL(ctx, tx, n); err != nil {
				return sqliteNodeWriteError(err, name)
			}
		} else {
			enrolled, seen := now, now
			// A node that has enrolled has not said how many sandboxes it takes, or whether it
			// takes any (that is its register): it takes none until then.
			n := Node{
				ID: id, Name: name, Endpoint: input.Endpoint, AgentEndpoint: agentEndpoint, State: "ready",
				VMMProfiles: append([]string(nil), profiles...), CapacityCPU: input.CapacityCPU,
				CapacityMemMiB: input.CapacityMemMiB, AcceptsWork: false,
				CertFingerprint: fp, CertSerial: serial, CertNotAfter: cert.notAfterPtr(),
				EnrolledAt: &enrolled, LastSeenAt: &seen, CreatedAt: now, UpdatedAt: now,
			}
			if err := insertNodeSQL(ctx, tx, n); err != nil {
				return sqliteNodeWriteError(err, name)
			}
		}
		_, _ = run(ctx, tx, `DELETE FROM node_cert_revocations WHERE fingerprint=?`, fp)
		if err := nodeEventSQL(ctx, tx, id, "node.enrolled", "enrollment", map[string]any{
			"cert_fingerprint": fp, "cert_serial": serial, "agent_endpoint": agentEndpoint,
			"enroll_auth": enrollAuthName(auth),
		}); err != nil {
			return fmt.Errorf("node event: %w", err)
		}
		out, err = getNodeSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) RotateNodeCert(ctx context.Context, nodeID string, cert CertMeta) (Node, error) {
	fp := strings.TrimSpace(cert.Fingerprint)
	serial := strings.TrimSpace(cert.Serial)
	if strings.TrimSpace(nodeID) == "" {
		return Node{}, fmt.Errorf("%w: node id required", ErrInvalidInput)
	}
	if fp == "" {
		return Node{}, fmt.Errorf("%w: cert_fingerprint required", ErrInvalidInput)
	}
	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if n.RevokedAt != nil {
			return fmt.Errorf("%w: node is revoked; re-enroll required", ErrConflict)
		}
		now := time.Now().UTC()
		oldFP, oldSerial := n.CertFingerprint, n.CertSerial
		if oldFP != "" && oldFP != fp {
			if _, err := run(ctx, tx, `
				INSERT INTO node_cert_revocations (fingerprint, node_id, serial, reason, revoked_at)
				VALUES (?,?,?,'rotated',?)
				ON CONFLICT (fingerprint) DO UPDATE SET revoked_at=excluded.revoked_at, reason=excluded.reason`,
				oldFP, nodeID, oldSerial, now); err != nil {
				return err
			}
		}
		n.CertFingerprint, n.CertSerial, n.CertNotAfter, n.UpdatedAt = fp, serial, cert.notAfterPtr(), now
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		_, _ = run(ctx, tx, `DELETE FROM node_cert_revocations WHERE fingerprint=?`, fp)
		if err := nodeEventSQL(ctx, tx, nodeID, "node.cert_rotated", "api", map[string]any{
			"cert_fingerprint": fp, "cert_serial": serial, "old_fingerprint": oldFP,
		}); err != nil {
			return err
		}
		out, err = getNodeSQL(ctx, tx, nodeID)
		return err
	})
	return out, err
}

func (s *SQLiteStore) RevokeNode(ctx context.Context, nodeID string) (Node, error) {
	if strings.TrimSpace(nodeID) == "" {
		return Node{}, fmt.Errorf("%w: node id required", ErrInvalidInput)
	}
	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		fp, serial := n.CertFingerprint, n.CertSerial
		n.RevokedAt, n.State, n.UpdatedAt = &now, "offline", now
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		if fp != "" {
			if _, err := run(ctx, tx, `
				INSERT INTO node_cert_revocations (fingerprint, node_id, serial, reason, revoked_at)
				VALUES (?,?,?,'revoked',?)
				ON CONFLICT (fingerprint) DO UPDATE SET revoked_at=excluded.revoked_at, reason=excluded.reason`,
				fp, nodeID, serial, now); err != nil {
				return err
			}
		}
		if err := nodeEventSQL(ctx, tx, nodeID, "node.revoked", "api", map[string]any{"cert_fingerprint": fp, "cert_serial": serial}); err != nil {
			return err
		}
		out, err = getNodeSQL(ctx, tx, nodeID)
		return err
	})
	return out, err
}

func (s *SQLiteStore) IsCertRevoked(ctx context.Context, fingerprint string) (bool, error) {
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return false, nil
	}
	var n int
	err := rowOf(ctx, s.db, `
		SELECT EXISTS(SELECT 1 FROM node_cert_revocations WHERE fingerprint=?)
		    OR EXISTS(SELECT 1 FROM nodes WHERE cert_fingerprint=? AND revoked_at IS NOT NULL)`, fp, fp).Scan(&n)
	return n == 1, err
}

func (s *SQLiteStore) HeartbeatNode(ctx context.Context, id string) (Node, error) {
	if strings.TrimSpace(id) == "" {
		return Node{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		if n.RevokedAt != nil {
			return errNodeRevoked(id)
		}
		prev := n.State
		now := time.Now().UTC()
		n.LastSeenAt, n.UpdatedAt, n.State = &now, now, "ready"
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		if prev == "offline" {
			_ = nodeEventSQL(ctx, tx, id, "node.online", "node-agent", map[string]any{})
		}
		out, err = getNodeSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) TouchNodePoll(ctx context.Context, id string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		// The throttle: a poll inside the window writes nothing.
		if n.LastSeenAt != nil && now.Sub(*n.LastSeenAt) < nodePollWriteEvery {
			return nil
		}
		prev := n.State
		seen := now
		n.LastSeenAt, n.UpdatedAt = &seen, now
		if n.RevokedAt == nil {
			n.State = "ready"
		}
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		if prev == "offline" && n.State == "ready" {
			_ = nodeEventSQL(ctx, tx, id, "node.online", "node-agent", map[string]any{})
		}
		return nil
	})
}

func (s *SQLiteStore) SetNodeCordoned(ctx context.Context, id string, cordoned bool) (Node, error) {
	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		n.Cordoned, n.UpdatedAt = cordoned, time.Now().UTC()
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		eventType := "node.uncordoned"
		if cordoned {
			eventType = "node.cordoned"
		}
		if err := nodeEventSQL(ctx, tx, id, eventType, "api", map[string]any{}); err != nil {
			return fmt.Errorf("node event: %w", err)
		}
		out, err = getNodeSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) SetNodeFence(ctx context.Context, id, endpoint, token string) (Node, error) {
	endpoint = strings.TrimSpace(endpoint)
	token = strings.TrimSpace(token)
	if endpoint == "" {
		token = ""
	}
	var out Node
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		n.FenceEndpoint, n.FenceToken, n.UpdatedAt = endpoint, token, time.Now().UTC()
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		// The event says a target was set or cleared, never what it is.
		eventType := "node.fence_set"
		if endpoint == "" {
			eventType = "node.fence_cleared"
		}
		if err := nodeEventSQL(ctx, tx, id, eventType, "api", map[string]any{}); err != nil {
			return fmt.Errorf("node event: %w", err)
		}
		out, err = getNodeSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := rowsOf(ctx, s.db, `SELECT `+nodeColumns+` FROM nodes ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Node, 0)
	for rows.Next() {
		n, err := scanNodeSQL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetNode(ctx context.Context, id string) (Node, error) {
	return getNodeSQL(ctx, s.db, id)
}

func (s *SQLiteStore) SetNodeDiskFree(ctx context.Context, id string, freeMiB int64) error {
	res, err := run(ctx, s.db, `UPDATE nodes SET disk_free_mib=? WHERE id=?`, freeMiB, id)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) EmitNodeEvent(ctx context.Context, nodeID, eventType, actor string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	err := nodeEventSQL(ctx, s.db, nodeID, eventType, actor, payload)
	if err != nil {
		var n int
		if rowOf(ctx, s.db, `SELECT count(*) FROM nodes WHERE id=?`, nodeID).Scan(&n) == nil && n == 0 {
			return ErrNotFound
		}
	}
	return err
}

// ---- liveness (ADR-0011) ----

func (s *SQLiteStore) MarkNodeOffline(ctx context.Context, id string, silentSince time.Time) (bool, error) {
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		if n.State == "offline" || n.RevokedAt != nil || !nodeLost(n, silentSince) {
			return nil
		}
		n.State, n.UpdatedAt = "offline", time.Now().UTC()
		if err := saveNodeSQL(ctx, tx, n); err != nil {
			return err
		}
		if err := nodeEventSQL(ctx, tx, id, "node.offline", "node-monitor", map[string]any{"silent_since": silentSince}); err != nil {
			return fmt.Errorf("node event: %w", err)
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *SQLiteStore) FailNodeSandboxes(ctx context.Context, nodeID, reason string, silentSince time.Time) ([]Sandbox, error) {
	out := []Sandbox{}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		n, err := getNodeSQL(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		// The node must still be lost when the rows are read: a heartbeat that lands first
		// keeps the sandboxes.
		if !nodeLost(n, silentSince) {
			return nil
		}
		states := occupyingStateNames()
		rows, err := rowsOf(ctx, tx, `SELECT `+sandboxColumns+` FROM sandboxes WHERE node_id=? AND state IN `+inList(len(states)),
			append([]any{nodeID}, strArgs(states)...)...)
		if err != nil {
			return err
		}
		victims, err := collectSandboxes(rows)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, sb := range victims {
			from := sb.State
			to := SandboxFailed
			switch from {
			case SandboxStopping:
				to = SandboxStopped
				sb.StoppedAt = &now
			case SandboxDeleting: // the disk went with the node
				to = SandboxDeleted
			}
			sb.State = to
			withdrawLocalNetFields(&sb)
			sb.StopReason = reason
			sb.StateVersion++
			sb.UpdatedAt = now
			if err := saveSandboxSQL(ctx, tx, sb); err != nil {
				return err
			}
			fromName := string(from)
			if err := emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: sb.ID, TenantID: sb.TenantID, EventType: "sandbox.node_lost",
				FromState: &fromName, ToState: strPtr(string(to)), Actor: "node-monitor",
				Payload: mustJSON(map[string]string{"node_id": nodeID, "reason": reason}),
			}); err != nil {
				return err
			}
			saved, err := getSandboxSQL(ctx, tx, sb.ID)
			if err != nil {
				return err
			}
			out = append(out, saved)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *SQLiteStore) FailUnassignedRequested(ctx context.Context, createdBefore time.Time, reason string) ([]Sandbox, error) {
	out := []Sandbox{}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := rowsOf(ctx, tx, `
			SELECT `+sandboxColumns+` FROM sandboxes
			WHERE state='requested' AND (node_id IS NULL OR node_id='') AND created_at < ?`, createdBefore)
		if err != nil {
			return err
		}
		victims, err := collectSandboxes(rows)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, sb := range victims {
			sb.State = SandboxFailed
			withdrawLocalNetFields(&sb)
			sb.StopReason = reason
			sb.StateVersion++
			sb.UpdatedAt = now
			if err := saveSandboxSQL(ctx, tx, sb); err != nil {
				return err
			}
			from := string(SandboxRequested)
			if err := emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: sb.ID, TenantID: sb.TenantID, EventType: "sandbox.unscheduled",
				FromState: &from, ToState: strPtr(string(SandboxFailed)), Actor: "node-monitor",
				Payload: mustJSON(map[string]string{"reason": reason}),
			}); err != nil {
				return err
			}
			saved, err := getSandboxSQL(ctx, tx, sb.ID)
			if err != nil {
				return err
			}
			out = append(out, saved)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
