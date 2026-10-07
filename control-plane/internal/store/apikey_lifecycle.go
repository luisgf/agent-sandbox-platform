package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const apiKeyColumns = `id, tenant_id, name, scope, key_prefix, secret_hash, last_used_at, expires_at, revoked_at, created_at`

func scanAPIKey(row pgx.Row) (ApiKey, error) {
	var k ApiKey
	err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.Scope, &k.KeyPrefix, &k.SecretHash,
		&k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt, &k.CreatedAt)
	return k, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func checkNewAPIKey(tenantID, name, keyPrefix, secretHash string) error {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(name) == "" || keyPrefix == "" || secretHash == "" {
		return fmt.Errorf("%w: tenant_id, name, key_prefix, secret_hash required", ErrInvalidInput)
	}
	return nil
}

// ---- memory store ----

func (m *MemoryStore) CreateAPIKey(ctx context.Context, tenantID, name, scope, keyPrefix, secretHash string, expiresAt *time.Time) (ApiKey, error) {
	if err := checkNewAPIKey(tenantID, name, keyPrefix, secretHash); err != nil {
		return ApiKey{}, err
	}
	scope, err := normalizeScope(scope)
	if err != nil {
		return ApiKey{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.apiKeys {
		if k.TenantID == tenantID && k.Name == name {
			return ApiKey{}, fmt.Errorf("%w: tenant %s already has an api key named %q", ErrConflict, tenantID, name)
		}
		if k.KeyPrefix == keyPrefix {
			return ApiKey{}, fmt.Errorf("%w: api key prefix %s is taken", ErrConflict, keyPrefix)
		}
	}
	k := ApiKey{
		ID: newID(), TenantID: tenantID, Name: name, Scope: scope, KeyPrefix: keyPrefix,
		SecretHash: secretHash, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC(),
	}
	m.apiKeys[secretHash] = k
	return k, nil
}

func (m *MemoryStore) ListAPIKeys(ctx context.Context, tenantID string) ([]ApiKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ApiKey, 0, len(m.apiKeys))
	for _, k := range m.apiKeys {
		if tenantID == "" || k.TenantID == tenantID {
			out = append(out, k)
		}
	}
	sortAPIKeys(out)
	return out, nil
}

func (m *MemoryStore) GetAPIKey(ctx context.Context, id string) (ApiKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, k := range m.apiKeys {
		if k.ID == id {
			return k, nil
		}
	}
	return ApiKey{}, ErrNotFound
}

func (m *MemoryStore) RevokeAPIKey(ctx context.Context, id string) (ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, k := range m.apiKeys {
		if k.ID == id {
			if k.RevokedAt == nil {
				now := time.Now().UTC()
				k.RevokedAt = &now
				m.apiKeys[hash] = k
			}
			return k, nil
		}
	}
	return ApiKey{}, ErrNotFound
}

func (m *MemoryStore) RotateAPIKey(ctx context.Context, id, keyPrefix, secretHash string) (ApiKey, error) {
	if keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: key_prefix, secret_hash required", ErrInvalidInput)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldHash string
	var found ApiKey
	for hash, k := range m.apiKeys {
		if k.ID == id {
			oldHash, found = hash, k
			continue
		}
		if k.KeyPrefix == keyPrefix {
			return ApiKey{}, fmt.Errorf("%w: api key prefix %s is taken", ErrConflict, keyPrefix)
		}
	}
	if found.ID == "" {
		return ApiKey{}, ErrNotFound
	}
	if found.RevokedAt != nil {
		return ApiKey{}, fmt.Errorf("%w: api key %s is revoked", ErrConflict, id)
	}
	delete(m.apiKeys, oldHash)
	found.KeyPrefix, found.SecretHash = keyPrefix, secretHash
	m.apiKeys[secretHash] = found
	return found, nil
}

func sortAPIKeys(keys []ApiKey) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && (keys[j].CreatedAt.Before(keys[j-1].CreatedAt) ||
			(keys[j].CreatedAt.Equal(keys[j-1].CreatedAt) && keys[j].ID < keys[j-1].ID)); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

// ---- postgres store ----

func (p *PostgresStore) CreateAPIKey(ctx context.Context, tenantID, name, scope, keyPrefix, secretHash string, expiresAt *time.Time) (ApiKey, error) {
	if err := checkNewAPIKey(tenantID, name, keyPrefix, secretHash); err != nil {
		return ApiKey{}, err
	}
	scope, err := normalizeScope(scope)
	if err != nil {
		return ApiKey{}, err
	}
	if err := p.ensureTenant(ctx, tenantID); err != nil {
		return ApiKey{}, err
	}
	k, err := scanAPIKey(p.pool.QueryRow(ctx, `
		INSERT INTO api_keys (id, tenant_id, name, scope, key_prefix, secret_hash, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+apiKeyColumns,
		newID(), tenantID, name, scope, keyPrefix, secretHash, expiresAt, time.Now().UTC()))
	if err != nil {
		if isUniqueViolation(err) {
			return ApiKey{}, fmt.Errorf("%w: tenant %s already has an api key named %q, or the prefix %s is taken", ErrConflict, tenantID, name, keyPrefix)
		}
		return ApiKey{}, err
	}
	return k, nil
}

func (p *PostgresStore) ListAPIKeys(ctx context.Context, tenantID string) ([]ApiKey, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+apiKeyColumns+` FROM api_keys
		WHERE ($1 = '' OR tenant_id = $1)
		ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ApiKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (p *PostgresStore) GetAPIKey(ctx context.Context, id string) (ApiKey, error) {
	k, err := scanAPIKey(p.pool.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApiKey{}, ErrNotFound
	}
	return k, err
}

func (p *PostgresStore) RevokeAPIKey(ctx context.Context, id string) (ApiKey, error) {
	k, err := scanAPIKey(p.pool.QueryRow(ctx, `
		UPDATE api_keys SET revoked_at = COALESCE(revoked_at, $2) WHERE id=$1
		RETURNING `+apiKeyColumns, id, time.Now().UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApiKey{}, ErrNotFound
	}
	return k, err
}

func (p *PostgresStore) RotateAPIKey(ctx context.Context, id, keyPrefix, secretHash string) (ApiKey, error) {
	if keyPrefix == "" || secretHash == "" {
		return ApiKey{}, fmt.Errorf("%w: key_prefix, secret_hash required", ErrInvalidInput)
	}
	k, err := scanAPIKey(p.pool.QueryRow(ctx, `
		UPDATE api_keys SET key_prefix=$2, secret_hash=$3
		WHERE id=$1 AND revoked_at IS NULL
		RETURNING `+apiKeyColumns, id, keyPrefix, secretHash))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := p.GetAPIKey(ctx, id); gerr != nil {
			return ApiKey{}, gerr // not found
		}
		return ApiKey{}, fmt.Errorf("%w: api key %s is revoked", ErrConflict, id)
	}
	if err != nil {
		if isUniqueViolation(err) {
			return ApiKey{}, fmt.Errorf("%w: api key prefix %s is taken", ErrConflict, keyPrefix)
		}
		return ApiKey{}, err
	}
	return k, nil
}
