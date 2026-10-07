package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *PostgresStore) IssueLocalNetGrant(ctx context.Context, id, dial string, now time.Time, ttl time.Duration) (string, time.Time, error) {
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
	sb, err := p.GetSandbox(ctx, id)
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

func (p *PostgresStore) HeartbeatLocalNet(ctx context.Context, id, grant, clientPub string, now time.Time) (Sandbox, error) {
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
	sb, err := p.GetSandbox(ctx, id)
	if err != nil {
		return Sandbox{}, err
	}
	if !sb.LocalNet {
		return Sandbox{}, fmt.Errorf("%w: local_net is off", ErrConflict)
	}
	if localNetTerminal(sb.State) {
		return Sandbox{}, fmt.Errorf("%w: sandbox not active", ErrConflict)
	}
	if sb.LocalNetGrantHash == "" || sb.LocalNetGrantExpiresAt == nil || !now.Before(*sb.LocalNetGrantExpiresAt) {
		_, _ = p.pool.Exec(ctx, `
			UPDATE sandboxes
			SET local_net_state='withdrawn', local_net_client_public='',
			    local_net_grant_hash='', local_net_grant_expires_at=NULL, updated_at=$2
			WHERE id=$1 AND local_net=true`, id, now)
		_ = p.EmitEvent(ctx, EmitEventInput{
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
	up, err := scanSandbox(p.pool.QueryRow(ctx, `
		UPDATE sandboxes
		SET local_net_state='up', local_net_client_public=$2, local_net_attached_at=$3, updated_at=$3
		WHERE id=$1 AND local_net=true
		RETURNING `+sandboxColumns, id, strings.TrimSpace(clientPub), now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	_ = p.EmitEvent(ctx, EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.local_net_up",
		Actor:     "local-agent",
		Payload:   mustJSON(map[string]any{"local_net_state": LocalNetUp, "public_egress": false}),
	})
	return up, nil
}

func (p *PostgresStore) WithdrawLocalNet(ctx context.Context, id string) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	sb, err := p.GetSandbox(ctx, id)
	if err != nil {
		return Sandbox{}, err
	}
	if !sb.LocalNet {
		return sb, nil
	}
	prev := sb.LocalNetState
	now := time.Now().UTC()
	withdrawn, err := scanSandbox(p.pool.QueryRow(ctx, `
		UPDATE sandboxes
		SET local_net_state='withdrawn', local_net_client_public='',
		    local_net_grant_hash='', local_net_grant_expires_at=NULL, updated_at=$2
		WHERE id=$1
		RETURNING `+sandboxColumns, id, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	if prev != LocalNetWithdrawn {
		_ = p.EmitEvent(ctx, EmitEventInput{
			SandboxID: id,
			TenantID:  sb.TenantID,
			EventType: "sandbox.local_net_withdrawn",
			Actor:     "api",
			Payload:   mustJSON(map[string]any{"reason": "detach", "public_egress": false}),
		})
	}
	return withdrawn, nil
}

func (p *PostgresStore) SetLocalNetNodePublic(ctx context.Context, id, publicKey string, tun LocalNetTunnel) (Sandbox, error) {
	if strings.TrimSpace(id) == "" {
		return Sandbox{}, fmt.Errorf("%w: id required", ErrInvalidInput)
	}
	if err := ValidateWGPublicKey(publicKey); err != nil {
		return Sandbox{}, err
	}
	if err := tun.Validate(); err != nil {
		return Sandbox{}, err
	}
	sb, err := p.GetSandbox(ctx, id)
	if err != nil {
		return Sandbox{}, err
	}
	if !sb.LocalNet {
		return Sandbox{}, fmt.Errorf("%w: local_net is off", ErrConflict)
	}
	now := time.Now().UTC()
	out, err := scanSandbox(p.pool.QueryRow(ctx, `
		UPDATE sandboxes
		SET local_net_node_public=$2, updated_at=$3,
		    local_net_listen_port=$4, local_net_node_addr=$5, local_net_client_addr=$6
		WHERE id=$1 AND local_net=true
		RETURNING `+sandboxColumns, id, strings.TrimSpace(publicKey), now, tun.ListenPort, tun.NodeAddr, tun.ClientAddr))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	return out, nil
}
