package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (p *PostgresStore) IssueLocalNetGrant(id, dial string, now time.Time, ttl time.Duration) (string, time.Time, error) {
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
	sb, err := p.GetSandbox(id)
	if err != nil {
		return "", time.Time{}, err
	}
	if !sb.LocalNet {
		return "", time.Time{}, fmt.Errorf("%w: local_net is off; attach does not enable it", ErrConflict)
	}
	if localNetTerminal(sb.State) {
		return "", time.Time{}, fmt.Errorf("%w: sandbox not active", ErrConflict)
	}
	exp := now.Add(ttl)
	ctx := context.Background()
	tag, err := p.pool.Exec(ctx, `
		UPDATE sandboxes
		SET local_net_grant_hash=$2, local_net_grant_expires_at=$3, updated_at=$4
		WHERE id=$1 AND local_net=true`, id, hash, exp, now)
	if err != nil {
		return "", time.Time{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", time.Time{}, ErrNotFound
	}
	return clear, exp, nil
}

func (p *PostgresStore) HeartbeatLocalNet(id, grant, clientPub string, now time.Time) (Sandbox, error) {
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
	sb, err := p.GetSandbox(id)
	if err != nil {
		return Sandbox{}, err
	}
	if !sb.LocalNet {
		return Sandbox{}, fmt.Errorf("%w: local_net is off", ErrConflict)
	}
	if localNetTerminal(sb.State) {
		return Sandbox{}, fmt.Errorf("%w: sandbox not active", ErrConflict)
	}
	ctx := context.Background()
	if sb.LocalNetGrantHash == "" || sb.LocalNetGrantExpiresAt == nil || !now.Before(*sb.LocalNetGrantExpiresAt) {
		_, _ = p.pool.Exec(ctx, `
			UPDATE sandboxes
			SET local_net_state='withdrawn', local_net_client_public='',
			    local_net_grant_hash='', local_net_grant_expires_at=NULL, updated_at=$2
			WHERE id=$1 AND local_net=true`, id, now)
		_ = p.EmitEvent(EmitEventInput{
			SandboxID: id,
			TenantID:  sb.TenantID,
			EventType: "sandbox.local_net_withdrawn",
			Actor:     "local-agent",
			Payload:   mustJSON(map[string]any{"reason": "grant_expired", "public_egress": false}),
		})
		return Sandbox{}, fmt.Errorf("%w: local_net grant expired", ErrUnauthorized)
	}
	if hashLocalNetGrant(grant) != sb.LocalNetGrantHash {
		return Sandbox{}, ErrUnauthorized
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE sandboxes
		SET local_net_state='up', local_net_client_public=$2, local_net_attached_at=$3, updated_at=$3
		WHERE id=$1 AND local_net=true`, id, strings.TrimSpace(clientPub), now)
	if err != nil {
		return Sandbox{}, err
	}
	if tag.RowsAffected() == 0 {
		return Sandbox{}, ErrNotFound
	}
	_ = p.EmitEvent(EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.local_net_up",
		Actor:     "local-agent",
		Payload:   mustJSON(map[string]any{"local_net_state": LocalNetUp, "public_egress": false}),
	})
	return p.GetSandbox(id)
}

func (p *PostgresStore) WithdrawLocalNet(id string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	sb, err := p.GetSandbox(id)
	if err != nil {
		return Sandbox{}, err
	}
	if !sb.LocalNet {
		return sb, nil
	}
	prev := sb.LocalNetState
	now := time.Now().UTC()
	ctx := context.Background()
	_, err = p.pool.Exec(ctx, `
		UPDATE sandboxes
		SET local_net_state='withdrawn', local_net_client_public='',
		    local_net_grant_hash='', local_net_grant_expires_at=NULL, updated_at=$2
		WHERE id=$1`, id, now)
	if err != nil {
		return Sandbox{}, err
	}
	if prev != LocalNetWithdrawn {
		_ = p.EmitEvent(EmitEventInput{
			SandboxID: id,
			TenantID:  sb.TenantID,
			EventType: "sandbox.local_net_withdrawn",
			Actor:     "api",
			Payload:   mustJSON(map[string]any{"reason": "detach", "public_egress": false}),
		})
	}
	return p.GetSandbox(id)
}
