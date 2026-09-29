package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

func TestPostgresStoreIntegration(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "1")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := ApplyMigrations(ctx, pool, migrations.FS, "."); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Isolate this run with a truncate of app tables (keep schema).
	_, _ = pool.Exec(ctx, `
		TRUNCATE sandbox_events, node_events, sandboxes, api_keys, nodes, tenants RESTART IDENTITY CASCADE`)

	pg := NewPostgresStore(pool)
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
