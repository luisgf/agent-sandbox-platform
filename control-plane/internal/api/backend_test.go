package api

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pgtest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

// The tests are written against the store contract, not against one store: each
// takes its store from newBackend, and this package's tests run on the memory store
// and, when DATABASE_URL names a Postgres server, a second time on Postgres. A
// difference between the two shows as a test that passes in one run and fails in
// the other (the failing test says which store it ran on).
//
// ASP_TEST_STORE=memory or =postgres runs only that pass.

// EnvTestStore restricts the run to one store.
const EnvTestStore = "ASP_TEST_STORE"

const (
	kindMemory   = "memory"
	kindPostgres = "postgres"
)

var (
	// currentKind is the store of the pass that is running.
	currentKind = kindMemory
	// pgPool is the pass's connection pool to its own database (Postgres only).
	pgPool *pgxpool.Pool
)

func TestMain(m *testing.M) {
	kinds, err := passes()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	code := 0
	for _, kind := range kinds {
		fmt.Fprintf(os.Stderr, "== internal/api tests on the %s store ==\n", kind)
		c, err := runPass(m, kind)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			c = 1
		}
		if c != 0 {
			code = c
		}
	}
	os.Exit(code)
}

// passes lists the stores to run the suite on.
func passes() ([]string, error) {
	switch only := strings.TrimSpace(os.Getenv(EnvTestStore)); only {
	case kindMemory:
		return []string{kindMemory}, nil
	case kindPostgres:
		if pgtest.Server() == "" {
			return nil, fmt.Errorf("%s=postgres needs %s", EnvTestStore, pgtest.EnvURL)
		}
		return []string{kindPostgres}, nil
	case "":
		if pgtest.Server() != "" {
			return []string{kindMemory, kindPostgres}, nil
		}
		return []string{kindMemory}, nil
	default:
		return nil, fmt.Errorf("%s=%q: want memory or postgres", EnvTestStore, only)
	}
}

func runPass(m *testing.M, kind string) (int, error) {
	currentKind = kind
	if kind != kindPostgres {
		return m.Run(), nil
	}
	dbURL, drop, err := pgtest.Database("asp_api_test")
	if err != nil {
		return 1, err
	}
	defer drop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The pool the control plane runs with, limits included.
	pool, err := store.NewPool(ctx, dbURL, store.DefaultDBTimeouts())
	if err != nil {
		return 1, fmt.Errorf("connect to the test database: %w", err)
	}
	defer pool.Close()
	if err := store.ApplyMigrations(ctx, pool, migrations.FS, "."); err != nil {
		return 1, fmt.Errorf("migrate the test database: %w", err)
	}
	pgPool = pool
	defer func() { pgPool = nil }()
	return m.Run(), nil
}

// backend is the store a test runs on, plus the few hooks only tests need: moving
// a clock the code under test reads, or forcing a state no API path sets. It is
// called mem in the tests for history; on a Postgres pass it is Postgres.
type backend struct {
	store.Store
	t *testing.T
}

// newBackend returns an empty store of the pass's kind.
func newBackend(t *testing.T) *backend {
	t.Helper()
	b := &backend{t: t}
	switch currentKind {
	case kindPostgres:
		b.Store = emptyPostgres(t)
	default:
		b.Store = store.NewMemoryStore()
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("(this test ran on the %s store)", currentKind)
		}
	})
	return b
}

// emptyPostgres empties every table of the pass's database and wraps its pool.
func emptyPostgres(t *testing.T) *store.PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := pgPool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("list tables: %v", err)
		}
		tables = append(tables, `"`+name+`"`)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) > 0 {
		if _, err := pgPool.Exec(ctx, `TRUNCATE `+strings.Join(tables, ", ")+` RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}
	return store.NewPostgresStore(pgPool)
}

// SetProvisionNodeID names the node ASP_AUTO_PROVISION places sandboxes on. On
// Postgres the node must exist (the sandbox row references it), so it is registered.
func (b *backend) SetProvisionNodeID(id string) {
	b.t.Helper()
	switch s := b.Store.(type) {
	case *store.MemoryStore:
		s.SetProvisionNodeID(id)
	case *store.PostgresStore:
		s.SetProvisionNodeID(id)
		if _, err := s.RegisterNode(context.Background(), store.RegisterNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
			b.t.Fatalf("register provision node %s: %v", id, err)
		}
	}
}

// SetStateForTest forces a sandbox state no API path sets, such as paused.
func (b *backend) SetStateForTest(id string, state store.SandboxState) {
	b.t.Helper()
	switch s := b.Store.(type) {
	case *store.MemoryStore:
		s.SetStateForTest(id, state)
	case *store.PostgresStore:
		b.exec(`UPDATE sandboxes SET state = $2 WHERE id = $1`, id, string(state))
	}
}

// SetLastActivityForTest pins a sandbox's last_activity_at.
func (b *backend) SetLastActivityForTest(id string, at time.Time) {
	b.t.Helper()
	switch s := b.Store.(type) {
	case *store.MemoryStore:
		s.SetLastActivityForTest(id, at)
	case *store.PostgresStore:
		b.exec(`UPDATE sandboxes SET last_activity_at = $2 WHERE id = $1`, id, at)
	}
}

// SetStoppedAtForTest pins a sandbox's stopped_at.
func (b *backend) SetStoppedAtForTest(id string, at time.Time) {
	b.t.Helper()
	switch s := b.Store.(type) {
	case *store.MemoryStore:
		s.SetStoppedAtForTest(id, at)
	case *store.PostgresStore:
		b.exec(`UPDATE sandboxes SET stopped_at = $2 WHERE id = $1`, id, at)
	}
}

// SetNodeLastSeenForTest pins a node's last_seen_at.
func (b *backend) SetNodeLastSeenForTest(id string, at time.Time) {
	b.t.Helper()
	switch s := b.Store.(type) {
	case *store.MemoryStore:
		s.SetNodeLastSeenForTest(id, at)
	case *store.PostgresStore:
		b.exec(`UPDATE nodes SET last_seen_at = $2 WHERE id = $1`, id, at)
	}
}

func (b *backend) exec(sql string, args ...any) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pgPool.Exec(ctx, sql, args...); err != nil {
		b.t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
	}
}
