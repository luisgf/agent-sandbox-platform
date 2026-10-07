package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDBTimeoutsFromEnv(t *testing.T) {
	for _, env := range []string{EnvDBStatementTimeout, EnvDBLockTimeout, EnvDBIdleTxTimeout} {
		t.Setenv(env, "")
	}
	got, err := DBTimeoutsFromEnv()
	if err != nil || got != DefaultDBTimeouts() {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	t.Setenv(EnvDBStatementTimeout, "45s")
	t.Setenv(EnvDBLockTimeout, "off")
	t.Setenv(EnvDBIdleTxTimeout, "0")
	got, err = DBTimeoutsFromEnv()
	if err != nil || got != (DBTimeouts{Statement: 45 * time.Second}) {
		t.Fatalf("set: %+v %v", got, err)
	}
	for _, bad := range []string{"soon", "-5s", "30"} {
		t.Setenv(EnvDBLockTimeout, bad)
		if _, err := DBTimeoutsFromEnv(); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// Every connection of the pool gets the limits, in milliseconds as Postgres reads
// a bare number; a limit the operator already put in DATABASE_URL is kept.
func TestPoolConfigSetsTheLimits(t *testing.T) {
	cfg, err := poolConfig("postgres://u:p@127.0.0.1:5432/asp?sslmode=disable", DBTimeouts{Statement: 30 * time.Second, Lock: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	rp := cfg.ConnConfig.RuntimeParams
	if rp["statement_timeout"] != "30000" || rp["lock_timeout"] != "1500" || rp["idle_in_transaction_session_timeout"] != "0" {
		t.Fatalf("runtime params: %v", rp)
	}
	own, err := poolConfig("postgres://u:p@127.0.0.1:5432/asp?sslmode=disable&statement_timeout=5000", DefaultDBTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	if own.ConnConfig.RuntimeParams["statement_timeout"] != "5000" {
		t.Fatalf("the URL's own limit was overridden: %v", own.ConnConfig.RuntimeParams)
	}
	if _, err := poolConfig("not a url ::", DefaultDBTimeouts()); err == nil {
		t.Fatal("a bad URL was accepted")
	}
}

// A cancelled or expired context ends a Postgres call at once, on a read and on
// a write that waits for the placement lock: a stalled database or a lock nobody
// releases no longer pins a handler for ever.
func TestPostgresCallsHonourTheContext(t *testing.T) {
	pg := newPostgresTestStore(t)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := pg.GetSandbox(cancelled, "no-such-sandbox"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled read: %v", err)
	}
	if _, err := pg.CreateSandbox(cancelled, CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled write: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("cancelled calls took %v", took)
	}

	// Someone holds the placement lock (another replica, a stuck transaction):
	// a create with a deadline gives up at the deadline.
	holder, err := pg.Pool().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_xact_lock($1)`, placementLockKey); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	start = time.Now()
	_, err = pg.CreateSandbox(ctx, CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a create blocked on the placement lock: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the create outlived its deadline by %v", took)
	}
}

// The pool's own limits cut what a context cannot reach: a statement that runs
// too long and a wait for a lock.
func TestPostgresPoolLimitsApply(t *testing.T) {
	pg := newPostgresTestStore(t)
	url := postgresTestURL(t)
	var pgErr *pgconn.PgError

	slow, err := NewPool(context.Background(), url, DBTimeouts{Statement: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	var got string
	if err := slow.QueryRow(context.Background(), `SHOW statement_timeout`).Scan(&got); err != nil || got != "300ms" {
		t.Fatalf("statement_timeout = %q (%v)", got, err)
	}
	start := time.Now()
	_, err = slow.Exec(context.Background(), `SELECT pg_sleep(5)`)
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" || time.Since(start) > 3*time.Second {
		t.Fatalf("a long statement: %v after %v", err, time.Since(start))
	}

	impatient, err := NewPool(context.Background(), url, DBTimeouts{Statement: 10 * time.Second, Lock: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer impatient.Close()
	holder, err := pg.Pool().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(context.Background(), `SELECT pg_advisory_xact_lock($1)`, int64(0x5ee4)); err != nil {
		t.Fatal(err)
	}
	tx, err := impatient.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	start = time.Now()
	_, err = tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock($1)`, int64(0x5ee4))
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || time.Since(start) > 3*time.Second {
		t.Fatalf("a lock wait: %v after %v", err, time.Since(start))
	}
}
