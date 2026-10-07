package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The server-side limits set on every connection of the pool (SET at connect).
// Without them a stalled query, a lock nobody releases or a transaction a
// crashed handler left open holds a connection, and the handlers behind it, for
// ever. A request's context bounds the client side (every Store method takes
// one); these bound the server side, which a cancelled request cannot reach once
// the connection is gone.
const (
	EnvDBStatementTimeout = "ASP_DB_STATEMENT_TIMEOUT"
	EnvDBLockTimeout      = "ASP_DB_LOCK_TIMEOUT"
	EnvDBIdleTxTimeout    = "ASP_DB_IDLE_TX_TIMEOUT"

	DefaultDBStatementTimeout = 30 * time.Second
	DefaultDBLockTimeout      = 10 * time.Second
	DefaultDBIdleTxTimeout    = 60 * time.Second
)

// DBTimeouts are those limits. Zero turns one off.
type DBTimeouts struct {
	Statement time.Duration // statement_timeout: any one statement
	Lock      time.Duration // lock_timeout: waiting for a lock (the placement advisory lock included)
	IdleTx    time.Duration // idle_in_transaction_session_timeout: a transaction nobody is using
}

// DefaultDBTimeouts are the limits used when the environment sets none.
func DefaultDBTimeouts() DBTimeouts {
	return DBTimeouts{Statement: DefaultDBStatementTimeout, Lock: DefaultDBLockTimeout, IdleTx: DefaultDBIdleTxTimeout}
}

// DBTimeoutsFromEnv reads ASP_DB_STATEMENT_TIMEOUT, ASP_DB_LOCK_TIMEOUT and
// ASP_DB_IDLE_TX_TIMEOUT (Go durations; 0 or "off" disables a limit).
func DBTimeoutsFromEnv() (DBTimeouts, error) {
	t := DefaultDBTimeouts()
	for _, f := range []struct {
		env string
		dst *time.Duration
	}{
		{EnvDBStatementTimeout, &t.Statement},
		{EnvDBLockTimeout, &t.Lock},
		{EnvDBIdleTxTimeout, &t.IdleTx},
	} {
		raw := strings.TrimSpace(os.Getenv(f.env))
		if raw == "" {
			continue
		}
		if strings.EqualFold(raw, "off") {
			*f.dst = 0
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return DBTimeouts{}, fmt.Errorf("%s=%q: want a duration like 30s, or 0 / off", f.env, raw)
		}
		*f.dst = d
	}
	return t, nil
}

// poolConfig parses url and sets the limits on its connections, except those the
// URL already names (?statement_timeout=…): the operator's own choice wins.
func poolConfig(url string, t DBTimeouts) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	set := func(name string, d time.Duration) {
		if _, own := cfg.ConnConfig.RuntimeParams[name]; own {
			return
		}
		cfg.ConnConfig.RuntimeParams[name] = strconv.FormatInt(d.Milliseconds(), 10) // milliseconds; 0 is off
	}
	set("statement_timeout", t.Statement)
	set("lock_timeout", t.Lock)
	set("idle_in_transaction_session_timeout", t.IdleTx)
	return cfg, nil
}

// NewPool opens the connection pool with the limits of t on every connection.
func NewPool(ctx context.Context, url string, t DBTimeouts) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(url, t)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}
