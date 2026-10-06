package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

// newPostgresTestStore connects to DATABASE_URL, applies migrations and empties the
// app tables. Tests using it skip when DATABASE_URL is unset (CI sets it).
func newPostgresTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := ApplyMigrations(ctx, pool, migrations.FS, "."); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Isolate this run with a truncate of app tables (keep schema).
	if _, err := pool.Exec(ctx, `
		TRUNCATE sandbox_events, node_events, node_cert_revocations, sandboxes, api_keys, nodes, tenants RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return NewPostgresStore(pool)
}

func TestPostgresStoreIntegration(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "1")
	pg := newPostgresTestStore(t)
	ctx := context.Background()
	if err := pg.EnsureBootstrapNode(ctx); err != nil {
		t.Fatal(err)
	}

	sb, err := pg.CreateSandbox(CreateSandboxInput{
		TenantID:  "tenant-pg",
		ImageRef:  "debian:bookworm",
		CPUMillis: 500,
		MemoryMiB: 256,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.State != SandboxRunning {
		t.Fatalf("state=%s", sb.State)
	}
	ev, err := pg.ListEvents(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) < 3 {
		t.Fatalf("events=%d", len(ev))
	}

	n, err := pg.RegisterNode(RegisterNodeInput{
		ID: "node-pg", Name: "node-pg", Endpoint: "http://127.0.0.1:9",
		CapacityCPU: 2, CapacityMemMiB: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "node-pg" {
		t.Fatalf("node=%+v", n)
	}

	secret := "integration-bootstrap-key"
	k, err := pg.EnsureAPIKey("default", "bootstrap", KeyPrefix(secret), HashAPIKeySecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	got, err := pg.LookupAPIKeyByHash(HashAPIKeySecret(secret))
	if err != nil || got.ID != k.ID {
		t.Fatalf("lookup key: %+v err=%v", got, err)
	}
}

func TestPostgresRevokedNodeStaysRevokedUntilReEnroll(t *testing.T) {
	pg := newPostgresTestStore(t)
	if _, err := pg.RegisterNode(RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.RevokeNode("n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.HeartbeatNode("n1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("heartbeat after revoke: want ErrConflict, got %v", err)
	}
	if _, err := pg.HeartbeatNode("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat unknown node: want ErrNotFound, got %v", err)
	}
	if _, err := pg.RegisterNode(RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("register after revoke: want ErrConflict, got %v", err)
	}
	n, err := pg.GetNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.State != "offline" || n.RevokedAt == nil {
		t.Fatalf("revoked node came back: %+v", n)
	}
	if _, err := pg.EnrollNode(EnrollNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}, CertMeta{Fingerprint: "fp-new"}); err != nil {
		t.Fatal(err)
	}
	if n, err := pg.HeartbeatNode("n1"); err != nil || n.State != "ready" {
		t.Fatalf("heartbeat after re-enroll: node=%+v err=%v", n, err)
	}
}
