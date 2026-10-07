package store

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres reports a violated constraint as an error of its own. The memory store
// answers the same call with one of the store's errors, and callers branch on those
// (they pick the HTTP status), so the Postgres store turns the violations a caller
// can cause into the same errors rather than let them out as a 500.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// Constraints the store names in its errors.
const (
	constraintNodeName       = "nodes_name_key"
	constraintKeyTenantName  = "api_keys_tenant_id_name_key"
	constraintKeyPrefix      = "api_keys_key_prefix_key"
	constraintKeySecretHash  = "api_keys_secret_hash_key"
	constraintEventSandbox   = "sandbox_events_sandbox_id_fkey"
	constraintNodeEventsNode = "node_events_node_id_fkey"
)

// pgViolation reports whether err is the Postgres error code, and on which constraint.
func pgViolation(err error, code string) (constraint string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == code {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// errNodeNameTaken is returned when a node registers or enrolls under the name
// another node has (names are unique).
func errNodeNameTaken(name string) error {
	return fmt.Errorf("%w: node name %q is used by another node", ErrConflict, name)
}

// nodeWriteError turns the error of a write to nodes into the store's own.
func nodeWriteError(err error, name string) error {
	if c, ok := pgViolation(err, pgUniqueViolation); ok && c == constraintNodeName {
		return errNodeNameTaken(name)
	}
	return err
}

func errKeyNameTaken(tenantID, name string) error {
	return fmt.Errorf("%w: tenant %s already has an api key named %q", ErrConflict, tenantID, name)
}

func errKeyPrefixTaken(prefix string) error {
	return fmt.Errorf("%w: api key prefix %s is taken", ErrConflict, prefix)
}

func errKeySecretTaken() error {
	return fmt.Errorf("%w: another api key has this secret", ErrConflict)
}

// apiKeyWriteError turns the error of a write to api_keys into the store's own.
func apiKeyWriteError(err error, tenantID, name, prefix string) error {
	c, ok := pgViolation(err, pgUniqueViolation)
	if !ok {
		return err
	}
	switch c {
	case constraintKeyTenantName:
		return errKeyNameTaken(tenantID, name)
	case constraintKeyPrefix:
		return errKeyPrefixTaken(prefix)
	case constraintKeySecretHash:
		return errKeySecretTaken()
	}
	return err
}
