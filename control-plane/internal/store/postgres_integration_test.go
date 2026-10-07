package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

// postgresTestURL is the database this binary created on the DATABASE_URL server
// (see TestMain); tests that need a database skip without it.
func postgresTestURL(t *testing.T) string {
	t.Helper()
	if testDatabaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres integration test")
	}
	return testDatabaseURL
}

// newPostgresTestStore connects to the test database, applies migrations and empties
// the app tables. Tests using it skip when DATABASE_URL is unset (CI sets it).
func newPostgresTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	dbURL := postgresTestURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The pool the control plane runs with, limits included.
	pool, err := NewPool(context.Background(), dbURL, DefaultDBTimeouts())
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
		TRUNCATE sandbox_events, node_events, node_cert_revocations, node_enroll_tokens, sandboxes, api_keys, nodes, tenants RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return NewPostgresStore(pool)
}

func testStoreIntegration(t *testing.T, pg interface {
	Store
	EnsureBootstrapNode(context.Context) error
}) {
	ctx := context.Background()
	if err := pg.EnsureBootstrapNode(ctx); err != nil {
		t.Fatal(err)
	}

	sb, err := pg.CreateSandbox(context.Background(), CreateSandboxInput{
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
	ev, err := pg.ListEvents(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) < 3 {
		t.Fatalf("events=%d", len(ev))
	}

	n, err := pg.RegisterNode(context.Background(), RegisterNodeInput{
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
	k, err := pg.EnsureAPIKey(context.Background(), "default", "bootstrap", APIKeyScopePlatform, KeyPrefix(secret), HashAPIKeySecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	got, err := pg.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(secret))
	if err != nil || got.ID != k.ID {
		t.Fatalf("lookup key: %+v err=%v", got, err)
	}
}

func TestPostgresStoreIntegration(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "1")
	testStoreIntegration(t, newPostgresTestStore(t))
}

func TestSQLiteStoreIntegration(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "1")
	testStoreIntegration(t, newSQLiteTestStore(t))
}

func testRevokedNodeStaysRevokedUntilReEnroll(t *testing.T, pg Store) {
	if _, err := pg.RegisterNode(context.Background(), RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.RevokeNode(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.HeartbeatNode(context.Background(), "n1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("heartbeat after revoke: want ErrConflict, got %v", err)
	}
	if _, err := pg.HeartbeatNode(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat unknown node: want ErrNotFound, got %v", err)
	}
	if _, err := pg.RegisterNode(context.Background(), RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("register after revoke: want ErrConflict, got %v", err)
	}
	n, err := pg.GetNode(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.State != "offline" || n.RevokedAt == nil {
		t.Fatalf("revoked node came back: %+v", n)
	}
	if _, err := pg.EnrollNode(context.Background(), EnrollNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}, CertMeta{Fingerprint: "fp-new"}, EnrollAuth{}); err != nil {
		t.Fatal(err)
	}
	if n, err := pg.HeartbeatNode(context.Background(), "n1"); err != nil || n.State != "ready" {
		t.Fatalf("heartbeat after re-enroll: node=%+v err=%v", n, err)
	}
}

func TestPostgresRevokedNodeStaysRevokedUntilReEnroll(t *testing.T) {
	testRevokedNodeStaysRevokedUntilReEnroll(t, newPostgresTestStore(t))
}

func TestSQLiteRevokedNodeStaysRevokedUntilReEnroll(t *testing.T) {
	testRevokedNodeStaysRevokedUntilReEnroll(t, newSQLiteTestStore(t))
}

func testAPIKeyScope(t *testing.T, pg Store) {
	secret := "scope-" + newID()
	k, err := pg.EnsureAPIKey(context.Background(), "tenant-scope", "scoped", APIKeyScopeTenant, KeyPrefix(secret), HashAPIKeySecret(secret))
	if err != nil {
		t.Fatal(err)
	}
	if k.Scope != APIKeyScopeTenant {
		t.Fatalf("created scope=%q", k.Scope)
	}
	got, err := pg.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(secret))
	if err != nil || got.Scope != APIKeyScopeTenant || got.TenantID != "tenant-scope" {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	// EnsureAPIKey promotes the same key to platform scope (the bootstrap key on upgrade).
	if _, err := pg.EnsureAPIKey(context.Background(), "tenant-scope", "scoped", APIKeyScopePlatform, KeyPrefix(secret), HashAPIKeySecret(secret)); err != nil {
		t.Fatal(err)
	}
	got, err = pg.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(secret))
	if err != nil || got.Scope != APIKeyScopePlatform {
		t.Fatalf("after promotion: %+v %v", got, err)
	}
	if _, err := pg.EnsureAPIKey(context.Background(), "tenant-scope", "bad", "root", "x", "y"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown scope: want ErrInvalidInput, got %v", err)
	}
}

func TestPostgresAPIKeyScope(t *testing.T) { testAPIKeyScope(t, newPostgresTestStore(t)) }

func TestSQLiteAPIKeyScope(t *testing.T) { testAPIKeyScope(t, newSQLiteTestStore(t)) }

// Enroll tokens are single use, even when enrollments race for one, and
// only a pinned token takes over a node that holds a certificate.
func testEnrollTokens(t *testing.T, pg Store) {
	future := time.Now().Add(time.Hour)
	tokFor := func(raw, nodeID string) EnrollAuth {
		t.Helper()
		if err := pg.CreateEnrollToken(context.Background(), EnrollToken{Hash: HashEnrollToken(raw), NodeID: nodeID, ExpiresAt: future, CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		return EnrollAuth{TokenHash: HashEnrollToken(raw)}
	}
	enroll := func(id, fp string, auth EnrollAuth) error {
		_, err := pg.EnrollNode(context.Background(), EnrollNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100"}, CertMeta{Fingerprint: fp}, auth)
		return err
	}

	shared := tokFor("race-"+newID(), "")
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) { errs <- enroll(fmt.Sprintf("race-%d", i), fmt.Sprintf("fp-race-%d", i), shared) }(i)
	}
	won := 0
	for i := 0; i < 8; i++ {
		switch err := <-errs; {
		case err == nil:
			won++
		case errors.Is(err, ErrEnrollTokenInvalid):
		default:
			t.Errorf("concurrent enroll: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d enrollments used one token", won)
	}

	if err := enroll("live", "fp-live-1", EnrollAuth{}); err != nil {
		t.Fatal(err)
	}
	if err := enroll("live", "fp-live-2", EnrollAuth{}); !errors.Is(err, ErrNodeEnrolled) {
		t.Fatalf("bootstrap re-enroll of a live node: want ErrNodeEnrolled, got %v", err)
	}
	if err := pg.CheckEnroll(context.Background(), "live", EnrollAuth{}); !errors.Is(err, ErrNodeEnrolled) {
		t.Fatalf("CheckEnroll: want ErrNodeEnrolled, got %v", err)
	}
	if err := enroll("other", "fp-other", tokFor("pin-"+newID(), "live")); !errors.Is(err, ErrEnrollTokenPinned) {
		t.Fatalf("pinned to another node: want ErrEnrollTokenPinned, got %v", err)
	}
	if err := enroll("live", "fp-live-3", tokFor("rekey-"+newID(), "live")); err != nil {
		t.Fatalf("pinned re-enroll: %v", err)
	}
	if revoked, err := pg.IsCertRevoked(context.Background(), "fp-live-1"); err != nil || !revoked {
		t.Fatalf("re-keying must revoke the previous certificate: revoked=%v err=%v", revoked, err)
	}
	expired := HashEnrollToken("expired-" + newID())
	if err := pg.CreateEnrollToken(context.Background(), EnrollToken{Hash: expired, ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := enroll("late", "fp-late", EnrollAuth{TokenHash: expired}); !errors.Is(err, ErrEnrollTokenInvalid) {
		t.Fatalf("expired token: want ErrEnrollTokenInvalid, got %v", err)
	}
}

func TestPostgresEnrollTokens(t *testing.T) { testEnrollTokens(t, newPostgresTestStore(t)) }

func TestSQLiteEnrollTokens(t *testing.T) { testEnrollTokens(t, newSQLiteTestStore(t)) }

func testNodeCertNotAfter(t *testing.T, pg Store) {
	first := time.Now().Add(365 * 24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := pg.EnrollNode(context.Background(), EnrollNodeInput{ID: "exp", AgentEndpoint: "http://127.0.0.1:9100"},
		CertMeta{Fingerprint: "fp-exp-1", NotAfter: first}, EnrollAuth{}); err != nil {
		t.Fatal(err)
	}
	n, err := pg.GetNode(context.Background(), "exp")
	if err != nil || n.CertNotAfter == nil || !n.CertNotAfter.Equal(first) {
		t.Fatalf("after enroll: cert_not_after=%v err=%v", n.CertNotAfter, err)
	}
	second := first.Add(30 * 24 * time.Hour)
	if _, err := pg.RotateNodeCert(context.Background(), "exp", CertMeta{Fingerprint: "fp-exp-2", NotAfter: second}); err != nil {
		t.Fatal(err)
	}
	if n, err = pg.GetNode(context.Background(), "exp"); err != nil || n.CertNotAfter == nil || !n.CertNotAfter.Equal(second) {
		t.Fatalf("after rotate: cert_not_after=%v err=%v", n.CertNotAfter, err)
	}
}

func TestPostgresNodeCertNotAfter(t *testing.T) { testNodeCertNotAfter(t, newPostgresTestStore(t)) }

func TestSQLiteNodeCertNotAfter(t *testing.T) { testNodeCertNotAfter(t, newSQLiteTestStore(t)) }

func testListEgressRulesForTenants(t *testing.T, pg Store) {
	if _, err := pg.PutEgressRules(context.Background(), "eg-t1", []EgressRule{{HostPattern: "api.github.com", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.PutEgressRules(context.Background(), "eg-t2", []EgressRule{{HostPattern: "a.example", Enabled: true}, {HostPattern: "b.example", Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	got, err := pg.ListEgressRulesForTenants(context.Background(), []string{"eg-t1", "eg-t2", "eg-none"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["eg-t1"]) != 1 || len(got["eg-t2"]) != 2 {
		t.Fatalf("rules: %+v", got)
	}
	if rules, ok := got["eg-none"]; !ok || len(rules) != 0 {
		t.Fatalf("a tenant without rules must have an empty entry: %+v", got)
	}
}

func TestPostgresListEgressRulesForTenants(t *testing.T) {
	testListEgressRulesForTenants(t, newPostgresTestStore(t))
}

func TestSQLiteListEgressRulesForTenants(t *testing.T) {
	testListEgressRulesForTenants(t, newSQLiteTestStore(t))
}

func testLocalNetNodeTunnel(t *testing.T, pg Store) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	registerPlacementNodes(t, pg, 0, "ln-node")
	on := true
	sb, err := pg.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, LocalNet: &on})
	if err != nil {
		t.Fatal(err)
	}
	tun := LocalNetTunnel{ListenPort: 50001, NodeAddr: "10.188.4.1/30", ClientAddr: "10.188.4.2/30"}
	if _, err := pg.SetLocalNetNodePublic(context.Background(), sb.ID, "ERERERERERERERERERERERERERERERERERERERERERE=", tun); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetSandbox(context.Background(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalNetListenPort != 50001 || got.LocalNetNodeAddr != "10.188.4.1/30" || got.LocalNetClientAddr != "10.188.4.2/30" {
		t.Fatalf("stored tunnel: %+v", got)
	}
	if _, err := pg.SetLocalNetNodePublic(context.Background(), sb.ID, "ERERERERERERERERERERERERERERERERERERERERERE=", LocalNetTunnel{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing tunnel parameters: want ErrInvalidInput, got %v", err)
	}
}

func TestPostgresLocalNetNodeTunnel(t *testing.T) { testLocalNetNodeTunnel(t, newPostgresTestStore(t)) }

func TestSQLiteLocalNetNodeTunnel(t *testing.T) { testLocalNetNodeTunnel(t, newSQLiteTestStore(t)) }

// countingTracer records the statements a store call sends (pgx.QueryTracer).
type countingTracer struct {
	mu   sync.Mutex
	sqls []string
}

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.sqls = append(c.sqls, data.SQL)
	c.mu.Unlock()
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// statements returns what was sent since the last call, without the
// transaction's begin/commit/rollback.
func (c *countingTracer) statements() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, q := range c.sqls {
		switch strings.ToLower(strings.TrimSpace(q)) {
		case "begin", "commit", "rollback":
			continue
		}
		out = append(out, strings.Join(strings.Fields(q), " "))
	}
	c.sqls = nil
	return out
}

// Writes return the row they changed instead of reading it back: one
// statement, plus the event insert where there is one.
func TestPostgresWritesAreOneStatementPlusTheEvent(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	newPostgresTestStore(t) // migrate and truncate
	cfg, err := pgxpool.ParseConfig(postgresTestURL(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := &countingTracer{}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pg := NewPostgresStore(pool)

	registerPlacementNodes(t, pg, 0, "trace-node")
	sb, err := pg.CreateSandbox(context.Background(), CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	tr.statements()

	check := func(what string, want int) {
		t.Helper()
		got := tr.statements()
		if len(got) != want {
			t.Errorf("%s sent %d statements, want %d:\n%s", what, len(got), want, strings.Join(got, "\n"))
		}
	}
	claimed, err := pg.ClaimSandbox(context.Background(), sb.ID, "trace-node")
	if err != nil || claimed.State != SandboxStarting || claimed.ID != sb.ID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	check("ClaimSandbox", 2)
	running, err := pg.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, "booted")
	if err != nil || running.State != SandboxRunning || running.StateVersion != claimed.StateVersion+1 {
		t.Fatalf("status: %+v %v", running, err)
	}
	check("UpdateSandboxStatus", 2)
	n, err := pg.HeartbeatNode(context.Background(), "trace-node")
	if err != nil || n.ID != "trace-node" || n.LastSeenAt == nil {
		t.Fatalf("heartbeat: %+v %v", n, err)
	}
	check("HeartbeatNode", 1)
	stopping, err := pg.StopSandbox(context.Background(), sb.ID, "user:a")
	if err != nil || stopping.State != SandboxStopping {
		t.Fatalf("stopping: %+v %v", stopping, err)
	}
	check("StopSandbox", 2)

	// A refused transition reads once to explain itself and changes nothing.
	if _, err := pg.UpdateSandboxStatus(context.Background(), sb.ID, SandboxRunning, "late"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("late running: %v", err)
	}
	tr.statements()
	if got, _ := pg.GetSandbox(context.Background(), sb.ID); got.State != SandboxStopping {
		t.Fatalf("a refused transition changed the row: %s", got.State)
	}
}

// The same lifecycle on Postgres: unique violations map to ErrConflict, a
// rotation replaces the secret in place, a revoked key stays listed.
func TestPostgresAPIKeyLifecycle(t *testing.T) {
	exerciseAPIKeyLifecycle(t, newPostgresTestStore(t))
}
