package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Events, API keys, egress rules, attestations and local-net of SQLiteStore.

func (s *SQLiteStore) EmitEvent(ctx context.Context, input EmitEventInput) error {
	return emitEventSQL(ctx, s.db, input)
}

func (s *SQLiteStore) ListEvents(ctx context.Context, sandboxID string) ([]SandboxEvent, error) {
	rows, err := rowsOf(ctx, s.db, `
		SELECT id, sandbox_id, tenant_id, event_type, from_state, to_state,
		       actor, actor_sub, request_id, payload, created_at
		FROM sandbox_events WHERE sandbox_id=? ORDER BY id ASC`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SandboxEvent, 0)
	for rows.Next() {
		var e SandboxEvent
		if err := rows.Scan(
			&e.ID, &e.SandboxID, &e.TenantID, &e.EventType,
			&e.FromState, &e.ToState, &e.Actor, &e.ActorSub, &e.RequestID,
			rawJSON{&e.Payload}, tsCol{&e.CreatedAt},
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- API keys ----

func scanAPIKeySQL(row scannable) (ApiKey, error) {
	var k ApiKey
	err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.Scope, &k.KeyPrefix, &k.SecretHash,
		tsPtrCol{&k.LastUsedAt}, tsPtrCol{&k.ExpiresAt}, tsPtrCol{&k.RevokedAt}, tsCol{&k.CreatedAt})
	return k, err
}

func (s *SQLiteStore) LookupAPIKeyByHash(ctx context.Context, secretHash string) (ApiKey, error) {
	k, err := scanAPIKeySQL(rowOf(ctx, s.db, `
		SELECT `+apiKeyColumns+` FROM api_keys
		WHERE secret_hash=? AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)`, secretHash, time.Now().UTC()))
	if isNoRows(err) {
		return ApiKey{}, ErrNotFound
	}
	return k, err
}

func (s *SQLiteStore) EnsureAPIKey(ctx context.Context, tenantID, name, scope, keyPrefix, secretHash string) (ApiKey, error) {
	if tenantID == "" || name == "" || keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: tenant_id, name, key_prefix, secret_hash required", ErrInvalidInput)
	}
	scope, err := normalizeScope(scope)
	if err != nil {
		return ApiKey{}, err
	}
	var out ApiKey
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.ensureTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		// Return existing by tenant+name if present.
		k, err := scanAPIKeySQL(rowOf(ctx, tx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE tenant_id=? AND name=?`, tenantID, name))
		if err == nil {
			// The same key again is returned as it is, revoked or not: a revocation holds across a
			// restart that ensures the same secret. A new secret (or prefix or scope) is the
			// operator's decision to have the key, and brings it back.
			if k.SecretHash != secretHash || k.KeyPrefix != keyPrefix || k.Scope != scope {
				if _, err := run(ctx, tx, `UPDATE api_keys SET secret_hash=?, key_prefix=?, scope=?, revoked_at=NULL WHERE id=?`,
					secretHash, keyPrefix, scope, k.ID); err != nil {
					return sqliteAPIKeyWriteError(err, tenantID, name, keyPrefix)
				}
				k.SecretHash, k.KeyPrefix, k.Scope, k.RevokedAt = secretHash, keyPrefix, scope, nil
			}
			out = k
			return nil
		}
		if !isNoRows(err) {
			return err
		}
		id := newID()
		now := time.Now().UTC()
		if _, err := run(ctx, tx, `
			INSERT INTO api_keys (id, tenant_id, name, scope, key_prefix, secret_hash, created_at)
			VALUES (?,?,?,?,?,?,?)`, id, tenantID, name, scope, keyPrefix, secretHash, now); err != nil {
			return sqliteAPIKeyWriteError(err, tenantID, name, keyPrefix)
		}
		out, err = scanAPIKeySQL(rowOf(ctx, tx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id=?`, id))
		return err
	})
	return out, err
}

func (s *SQLiteStore) CountAPIKeys(ctx context.Context) (int64, error) {
	var n int64
	err := rowOf(ctx, s.db, `SELECT count(*) FROM api_keys WHERE revoked_at IS NULL`).Scan(&n)
	return n, err
}

func (s *SQLiteStore) TouchAPIKey(ctx context.Context, id string) error {
	// The middleware already writes at most once a minute per key and process; the guard keeps
	// several control planes on one file to the same pace. The write is throttled, so no row
	// changed does not mean there is no key.
	now := time.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := run(ctx, tx, `
			UPDATE api_keys SET last_used_at=?
			WHERE id=? AND (last_used_at IS NULL OR last_used_at < ?)`, now, id, now.Add(-time.Minute)); err != nil {
			return err
		}
		var n int
		if err := rowOf(ctx, tx, `SELECT count(*) FROM api_keys WHERE id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *SQLiteStore) CreateAPIKey(ctx context.Context, tenantID, name, scope, keyPrefix, secretHash string, expiresAt *time.Time) (ApiKey, error) {
	if err := checkNewAPIKey(tenantID, name, keyPrefix, secretHash); err != nil {
		return ApiKey{}, err
	}
	scope, err := normalizeScope(scope)
	if err != nil {
		return ApiKey{}, err
	}
	var out ApiKey
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.ensureTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		id := newID()
		if _, err := run(ctx, tx, `
			INSERT INTO api_keys (id, tenant_id, name, scope, key_prefix, secret_hash, expires_at, created_at)
			VALUES (?,?,?,?,?,?,?,?)`,
			id, tenantID, name, scope, keyPrefix, secretHash, expiresAt, time.Now().UTC()); err != nil {
			return sqliteAPIKeyWriteError(err, tenantID, name, keyPrefix)
		}
		var err error
		out, err = scanAPIKeySQL(rowOf(ctx, tx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id=?`, id))
		return err
	})
	return out, err
}

func (s *SQLiteStore) ListAPIKeys(ctx context.Context, tenantID string) ([]ApiKey, error) {
	rows, err := rowsOf(ctx, s.db, `
		SELECT `+apiKeyColumns+` FROM api_keys
		WHERE (? = '' OR tenant_id = ?)
		ORDER BY created_at, id`, tenantID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApiKey{}
	for rows.Next() {
		k, err := scanAPIKeySQL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetAPIKey(ctx context.Context, id string) (ApiKey, error) {
	k, err := scanAPIKeySQL(rowOf(ctx, s.db, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id=?`, id))
	if isNoRows(err) {
		return ApiKey{}, ErrNotFound
	}
	return k, err
}

func (s *SQLiteStore) RevokeAPIKey(ctx context.Context, id string) (ApiKey, error) {
	k, err := scanAPIKeySQL(rowOf(ctx, s.db, `
		UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id=?
		RETURNING `+apiKeyColumns, time.Now().UTC(), id))
	if isNoRows(err) {
		return ApiKey{}, ErrNotFound
	}
	return k, err
}

func (s *SQLiteStore) RotateAPIKey(ctx context.Context, id, keyPrefix, secretHash string) (ApiKey, error) {
	if keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: key_prefix, secret_hash required", ErrInvalidInput)
	}
	var out ApiKey
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cur, err := scanAPIKeySQL(rowOf(ctx, tx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id=?`, id))
		if isNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.RevokedAt != nil {
			return fmt.Errorf("%w: api key %s is revoked", ErrConflict, id)
		}
		if _, err := run(ctx, tx, `UPDATE api_keys SET key_prefix=?, secret_hash=? WHERE id=?`, keyPrefix, secretHash, id); err != nil {
			return sqliteAPIKeyWriteError(err, "", "", keyPrefix)
		}
		out, err = scanAPIKeySQL(rowOf(ctx, tx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id=?`, id))
		return err
	})
	return out, err
}

// ---- tenant egress rules ----

func (s *SQLiteStore) ListEgressRules(ctx context.Context, tenantID string) ([]EgressRule, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, fmt.Errorf("%w: tenant_id required", ErrInvalidInput)
	}
	return listEgressRulesSQL(ctx, s.db, tenantID)
}

func listEgressRulesSQL(ctx context.Context, q runner, tenantID string) ([]EgressRule, error) {
	rows, err := rowsOf(ctx, q, `
		SELECT id, tenant_id, host_pattern, port, enabled
		FROM tenant_egress_rules WHERE tenant_id=?
		ORDER BY host_pattern, port`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EgressRule, 0)
	for rows.Next() {
		var r EgressRule
		if err := rows.Scan(&r.ID, &r.TenantID, &r.HostPattern, &r.Port, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListEgressRulesForTenants(ctx context.Context, tenantIDs []string) (map[string][]EgressRule, error) {
	out := make(map[string][]EgressRule, len(tenantIDs))
	for _, t := range tenantIDs {
		out[t] = []EgressRule{}
	}
	if len(tenantIDs) == 0 {
		return out, nil
	}
	rows, err := rowsOf(ctx, s.db, `
		SELECT id, tenant_id, host_pattern, port, enabled
		FROM tenant_egress_rules WHERE tenant_id IN `+inList(len(tenantIDs))+`
		ORDER BY tenant_id, host_pattern, port`, strArgs(tenantIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r EgressRule
		if err := rows.Scan(&r.ID, &r.TenantID, &r.HostPattern, &r.Port, &r.Enabled); err != nil {
			return nil, err
		}
		out[r.TenantID] = append(out[r.TenantID], r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) PutEgressRules(ctx context.Context, tenantID string, rules []EgressRule) ([]EgressRule, error) {
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
		id := r.ID
		if id == "" {
			id = newID()
		}
		cleaned = append(cleaned, EgressRule{ID: id, TenantID: tenantID, HostPattern: hp, Port: cloneInt(r.Port), Enabled: r.Enabled})
	}
	var out []EgressRule
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.ensureTenant(ctx, tx, tenantID); err != nil {
			return err
		}
		if _, err := run(ctx, tx, `DELETE FROM tenant_egress_rules WHERE tenant_id=?`, tenantID); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, r := range cleaned {
			if _, err := run(ctx, tx, `
				INSERT INTO tenant_egress_rules (id, tenant_id, host_pattern, port, enabled, created_at, updated_at)
				VALUES (?,?,?,?,?,?,?)`, r.ID, r.TenantID, r.HostPattern, r.Port, r.Enabled, now, now); err != nil {
				return err
			}
		}
		var err error
		out, err = listEgressRulesSQL(ctx, tx, tenantID)
		return err
	})
	return out, err
}

// ---- attestations ----

func (s *SQLiteStore) PutAttestation(ctx context.Context, input PutAttestationInput) (AttestationRecord, error) {
	if strings.TrimSpace(input.SandboxID) == "" {
		return AttestationRecord{}, fmt.Errorf("%w: sandbox_id required", ErrInvalidInput)
	}
	var out AttestationRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := getSandboxSQL(ctx, tx, input.SandboxID); err != nil {
			return err
		}
		bundle := input.Bundle
		if len(bundle) == 0 {
			bundle = json.RawMessage(`{}`)
		}
		if _, err := run(ctx, tx, `
			INSERT INTO sandbox_attestations (
				sandbox_id, node_id, image_digest, vmm_profile, cid, statement_ts,
				alg, key_id, signature, bundle, received_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (sandbox_id) DO UPDATE SET
				node_id=excluded.node_id,
				image_digest=excluded.image_digest,
				vmm_profile=excluded.vmm_profile,
				cid=excluded.cid,
				statement_ts=excluded.statement_ts,
				alg=excluded.alg,
				key_id=excluded.key_id,
				signature=excluded.signature,
				bundle=excluded.bundle,
				received_at=excluded.received_at`,
			input.SandboxID, input.NodeID, input.ImageDigest, input.VMMProfile, int(input.CID),
			input.StatementTS, input.Alg, input.KeyID, input.Signature, bundle, time.Now().UTC(),
		); err != nil {
			return err
		}
		var err error
		out, err = getAttestationSQL(ctx, tx, input.SandboxID)
		return err
	})
	return out, err
}

func (s *SQLiteStore) GetAttestation(ctx context.Context, sandboxID string) (AttestationRecord, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return AttestationRecord{}, fmt.Errorf("%w: sandbox_id required", ErrInvalidInput)
	}
	return getAttestationSQL(ctx, s.db, sandboxID)
}

func getAttestationSQL(ctx context.Context, q runner, sandboxID string) (AttestationRecord, error) {
	var rec AttestationRecord
	var cid int
	err := rowOf(ctx, q, `
		SELECT sandbox_id, node_id, image_digest, vmm_profile, cid, statement_ts,
		       alg, key_id, signature, bundle, received_at
		FROM sandbox_attestations WHERE sandbox_id=?`, sandboxID).Scan(
		&rec.SandboxID, &rec.NodeID, &rec.ImageDigest, &rec.VMMProfile, &cid, tsCol{&rec.StatementTS},
		&rec.Alg, &rec.KeyID, &rec.Signature, rawJSON{&rec.Bundle}, tsCol{&rec.ReceivedAt},
	)
	if isNoRows(err) {
		return AttestationRecord{}, ErrNotFound
	}
	if err != nil {
		return AttestationRecord{}, err
	}
	rec.CID = uint32(cid)
	if rec.Bundle == nil {
		rec.Bundle = json.RawMessage(`{}`)
	}
	return rec, nil
}

// ---- local-net (ADR-0010) ----

func (s *SQLiteStore) IssueLocalNetGrant(ctx context.Context, id, dial string, now time.Time, ttl time.Duration) (string, time.Time, error) {
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
	_ = dial
	exp := now.Add(ttl)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sb.LocalNet {
			return fmt.Errorf("%w: local_net is off; attach does not enable it", ErrConflict)
		}
		if localNetTerminal(sb.State) {
			return fmt.Errorf("%w: sandbox not active", ErrConflict)
		}
		sb.LocalNetGrantHash, sb.LocalNetGrantExpiresAt, sb.UpdatedAt = hash, &exp, now
		return saveSandboxSQL(ctx, tx, sb)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return clear, exp, nil
}

func (s *SQLiteStore) HeartbeatLocalNet(ctx context.Context, id, grant, clientPub string, now time.Time) (Sandbox, error) {
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
	var out Sandbox
	var bad error // returned after the transaction commits: an expired grant is still withdrawn
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sb.LocalNet {
			return fmt.Errorf("%w: local_net is off", ErrConflict)
		}
		if localNetTerminal(sb.State) {
			return fmt.Errorf("%w: sandbox not active", ErrConflict)
		}
		if sb.LocalNetGrantHash == "" || sb.LocalNetGrantExpiresAt == nil || !now.Before(*sb.LocalNetGrantExpiresAt) {
			withdrawLocalNetFields(&sb)
			sb.UpdatedAt = now
			if err := saveSandboxSQL(ctx, tx, sb); err != nil {
				return err
			}
			_ = emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: id, TenantID: sb.TenantID, EventType: "sandbox.local_net_withdrawn",
				Actor: "local-agent", Payload: mustJSON(map[string]any{"reason": "grant_expired", "public_egress": false}),
			})
			bad = fmt.Errorf("%w: local_net grant expired", ErrUnauthorized)
			return nil
		}
		if hashLocalNetGrant(grant) != sb.LocalNetGrantHash {
			return ErrUnauthorized
		}
		sb.LocalNetState = LocalNetUp
		sb.LocalNetClientPublic = strings.TrimSpace(clientPub)
		attached := now
		sb.LocalNetAttachedAt = &attached
		sb.UpdatedAt = now
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		_ = emitEventSQL(ctx, tx, EmitEventInput{
			SandboxID: id, TenantID: sb.TenantID, EventType: "sandbox.local_net_up",
			Actor: "local-agent", Payload: mustJSON(map[string]any{"local_net_state": LocalNetUp, "public_egress": false}),
		})
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	if err != nil {
		return Sandbox{}, err
	}
	if bad != nil {
		return Sandbox{}, bad
	}
	return out, nil
}

func (s *SQLiteStore) WithdrawLocalNet(ctx context.Context, id string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		prev := sb.LocalNetState
		if sb.LocalNet {
			withdrawLocalNetFields(&sb)
		}
		sb.UpdatedAt = time.Now().UTC()
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		if sb.LocalNet && prev != LocalNetWithdrawn {
			_ = emitEventSQL(ctx, tx, EmitEventInput{
				SandboxID: id, TenantID: sb.TenantID, EventType: "sandbox.local_net_withdrawn",
				Actor: "api", Payload: mustJSON(map[string]any{"reason": "detach", "public_egress": false}),
			})
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}

func (s *SQLiteStore) SetLocalNetNodePublic(ctx context.Context, id, publicKey string, tun LocalNetTunnel) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if err := ValidateWGPublicKey(publicKey); err != nil {
		return Sandbox{}, err
	}
	if err := tun.Validate(); err != nil {
		return Sandbox{}, err
	}
	var out Sandbox
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sb, err := getSandboxSQL(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sb.LocalNet {
			return fmt.Errorf("%w: local_net is off", ErrConflict)
		}
		sb.LocalNetNodePublic = strings.TrimSpace(publicKey)
		sb.LocalNetListenPort, sb.LocalNetNodeAddr, sb.LocalNetClientAddr = tun.ListenPort, tun.NodeAddr, tun.ClientAddr
		sb.UpdatedAt = time.Now().UTC()
		if err := saveSandboxSQL(ctx, tx, sb); err != nil {
			return err
		}
		out, err = getSandboxSQL(ctx, tx, id)
		return err
	})
	return out, err
}
