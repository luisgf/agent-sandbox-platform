package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

// newSQLiteTestStore opens an empty database in the test's own directory.
func newSQLiteTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "asp.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSQLiteParityWithMemory plays the parity script on SQLite: it needs no server, so it
// runs everywhere.
func TestSQLiteParityWithMemory(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	script := paritySteps()
	mem := play(t, NewMemoryStore(), script)
	lite := play(t, newSQLiteTestStore(t), script)
	compare(t, script, "memory", mem, "sqlite", lite)
}

// The store tests the other stores share, run on SQLite. Each of these has a Memory and a
// Postgres twin in its own file; the hooks set a column by hand, as the Postgres ones do.

func (s *SQLiteStore) setForTest(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), query, sqlArgs(args)...); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteBootedAt(t *testing.T)       { testBootedAt(t, newSQLiteTestStore(t)) }
func TestSQLiteCountSandboxes(t *testing.T) { testCountSandboxes(t, newSQLiteTestStore(t)) }
func TestSQLiteNodeSchedulingAttributes(t *testing.T) {
	testNodeSchedulingAttributes(t, newSQLiteTestStore(t))
}
func TestSQLiteNodeEgressEnforced(t *testing.T) { testNodeEgressEnforced(t, newSQLiteTestStore(t)) }

func TestSQLiteNodeLoss(t *testing.T) {
	s := newSQLiteTestStore(t)
	testNodeLoss(t, s, livenessHooks{
		setLastSeen: func(id string, ts time.Time) { s.setForTest(t, `UPDATE nodes SET last_seen_at=? WHERE id=?`, ts, id) },
		unassign: func(id string, createdAt time.Time) {
			s.setForTest(t, `UPDATE sandboxes SET node_id=NULL, created_at=? WHERE id=?`, createdAt, id)
		},
	})
}
func TestSQLiteAgentRestartOrphans(t *testing.T) { testAgentRestartOrphans(t, newSQLiteTestStore(t)) }
func TestSQLiteAgentRestartKeepsAdoptedSandboxes(t *testing.T) {
	testAgentRestartKeepsAdoptedSandboxes(t, newSQLiteTestStore(t))
}

func TestSQLiteConcurrentCreatesRespectCapacity(t *testing.T) {
	testConcurrentCreatesRespectCapacity(t, newSQLiteTestStore(t))
}
func TestSQLitePlacementPolicyAndPins(t *testing.T) {
	s := newSQLiteTestStore(t)
	testPlacementPolicyAndPins(t, s, s.SetSchedConfig)
}
func TestSQLiteBinpackStacksSandboxes(t *testing.T) {
	s := newSQLiteTestStore(t)
	testBinpackStacksSandboxes(t, s, s.SetSchedConfig)
}
func TestSQLiteClaimOnlyByAssignedNode(t *testing.T) {
	testClaimOnlyByAssignedNode(t, newSQLiteTestStore(t))
}
func TestSQLiteTouchNodePoll(t *testing.T)  { testTouchNodePoll(t, newSQLiteTestStore(t)) }
func TestSQLiteCordonAndUsage(t *testing.T) { testCordonAndUsage(t, newSQLiteTestStore(t)) }
func TestSQLiteAgentTransitionsAndAssignment(t *testing.T) {
	testAgentTransitionsAndAssignment(t, newSQLiteTestStore(t))
}
func TestSQLiteEnrolledNodeTakesNothingUntilItRegisters(t *testing.T) {
	testEnrolledNodeTakesNothingUntilItRegisters(t, newSQLiteTestStore(t))
}

func TestSQLiteExpireStoppedSandboxes(t *testing.T) {
	s := newSQLiteTestStore(t)
	testExpireStoppedSandboxes(t, s, func(id string, at time.Time) {
		s.setForTest(t, `UPDATE sandboxes SET stopped_at=? WHERE id=?`, at, id)
	})
}
func TestSQLiteEvictStoppedOverCap(t *testing.T) {
	s := newSQLiteTestStore(t)
	testEvictStoppedOverCap(t, s, func(id string, at time.Time) {
		s.setForTest(t, `UPDATE sandboxes SET stopped_at=? WHERE id=?`, at, id)
	})
}
func TestSQLiteNodeDiskFreeAndStoppedCount(t *testing.T) {
	testNodeDiskFreeAndStoppedCount(t, newSQLiteTestStore(t))
}

func TestSQLiteStopThenResume(t *testing.T) { testStopThenResume(t, newSQLiteTestStore(t)) }
func TestSQLiteStopPlans(t *testing.T)      { testStopPlans(t, newSQLiteTestStore(t)) }
func TestSQLiteResumeNeedsRoomOnItsNode(t *testing.T) {
	testResumeNeedsRoomOnItsNode(t, newSQLiteTestStore(t))
}
func TestSQLiteConcurrentResumesRespectCapacity(t *testing.T) {
	testConcurrentResumesRespectCapacity(t, newSQLiteTestStore(t))
}
func TestSQLiteDeleteEndsAtDeleted(t *testing.T) { testDeleteEndsAtDeleted(t, newSQLiteTestStore(t)) }
func TestSQLiteStatusDetail(t *testing.T)        { testStatusDetail(t, newSQLiteTestStore(t)) }
func TestSQLiteVMMExitedReport(t *testing.T)     { testVMMExitedReport(t, newSQLiteTestStore(t)) }
func TestSQLiteIdleReapedSandboxResumes(t *testing.T) {
	s := newSQLiteTestStore(t)
	testIdleReapedSandboxResumes(t, s, func(id string, at time.Time) {
		s.setForTest(t, `UPDATE sandboxes SET last_activity_at=? WHERE id=?`, at, id)
	})
}
func TestSQLiteLostNodeEndsDeletes(t *testing.T) {
	s := newSQLiteTestStore(t)
	testLostNodeEndsDeletes(t, s, func(id string, ts time.Time) {
		s.setForTest(t, `UPDATE nodes SET last_seen_at=? WHERE id=?`, ts, id)
	})
}
func TestSQLiteDeletingHoldsItsSlotUntilDeleted(t *testing.T) {
	testDeletingHoldsItsSlotUntilDeleted(t, newSQLiteTestStore(t))
}

// The same lifecycle on SQLite: unique violations map to ErrConflict, a rotation replaces the
// secret in place, a revoked key stays listed.
func TestSQLiteAPIKeyLifecycle(t *testing.T) { exerciseAPIKeyLifecycle(t, newSQLiteTestStore(t)) }

// What was written is there after the file is opened again, and a second open does not run
// the migrations twice.
func TestSQLiteKeepsItsDataAcrossOpens(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	path := filepath.Join(t.TempDir(), "asp.db")
	ctx := context.Background()
	s, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	registerPlacementNodes(t, s, 0, "keep-node")
	sb, err := s.CreateSandbox(ctx, CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.EnsureAPIKey(ctx, "t", "k", APIKeyScopePlatform, KeyPrefix("secret-1"), HashAPIKeySecret("secret-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetSandbox(ctx, sb.ID)
	if err != nil || got.State != SandboxRequested || got.NodeID == nil || *got.NodeID != "keep-node" {
		t.Fatalf("sandbox after reopen: %+v %v", got, err)
	}
	if ev, err := s.ListEvents(ctx, sb.ID); err != nil || len(ev) < 2 {
		t.Fatalf("events after reopen: %v %v", ev, err)
	}
	if k, err := s.LookupAPIKeyByHash(ctx, HashAPIKeySecret("secret-1")); err != nil || k.ID != key.ID {
		t.Fatalf("key after reopen: %+v %v", k, err)
	}
	var applied int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("migrations recorded: %d %v", applied, err)
	}
}

// The SQLite schema is kept in step with the Postgres one: a migration the Postgres side gains
// needs its twin here, with the same number, or a database made by one would differ from the other.
func TestSQLiteMigrationsMatchPostgres(t *testing.T) {
	numbers := func(fsys fs.FS, dir string) []int {
		t.Helper()
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			n, err := strconv.Atoi(e.Name()[:3])
			if err != nil {
				t.Fatalf("%s does not start with a number", e.Name())
			}
			out = append(out, n)
		}
		sort.Ints(out)
		return out
	}
	pg, lite := numbers(migrations.FS, "."), numbers(migrations.SQLiteFS, "sqlite")
	if len(lite) == 0 || lite[0] != 24 {
		t.Fatalf("the SQLite migrations start with the baseline 024 (the Postgres schema as of 024); they are %v", lite)
	}
	have := map[int]bool{}
	for _, n := range lite {
		have[n] = true
	}
	for _, n := range pg {
		if n > 24 && !have[n] {
			t.Errorf("Postgres migration %03d has no SQLite twin in migrations/sqlite", n)
		}
	}
	top := pg[len(pg)-1]
	for _, n := range lite {
		if n > top {
			t.Errorf("SQLite migration %03d has no Postgres twin", n)
		}
	}
}

// Times are compared as text, which only works while every one is written in the same width.
func TestSQLiteStoresEveryTimeInOneWidth(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := newSQLiteTestStore(t)
	play(t, s, paritySteps()) // touches every table the store writes
	width := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z$`)
	ctx := context.Background()
	tables, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'schema_migrations'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	checked := 0
	for _, table := range names {
		cols, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('`+table+`')`)
		if err != nil {
			t.Fatal(err)
		}
		var timeCols []string
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(c, "_at") || strings.HasSuffix(c, "_until") || strings.HasSuffix(c, "_ts") {
				timeCols = append(timeCols, c)
			}
		}
		cols.Close()
		for _, c := range timeCols {
			rows, err := s.db.QueryContext(ctx, `SELECT `+c+` FROM `+table+` WHERE `+c+` IS NOT NULL`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					t.Fatal(err)
				}
				checked++
				if !width.MatchString(v) {
					t.Errorf("%s.%s holds %q", table, c, v)
				}
			}
			rows.Close()
		}
	}
	if checked < 50 {
		t.Fatalf("only %d times were checked: does the script touch the tables?", checked)
	}
}

// Two control planes on one file queue behind each other instead of failing.
func TestSQLiteTwoStoresOneFile(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	path := filepath.Join(t.TempDir(), "asp.db")
	ctx := context.Background()
	a, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	registerPlacementNodes(t, a, 0, "shared-node")
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		st := Store(a)
		if i%2 == 1 {
			st = b
		}
		go func() {
			_, err := st.CreateSandbox(ctx, CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64})
			errs <- err
		}()
	}
	for i := 0; i < 40; i++ {
		if err := <-errs; err != nil {
			t.Errorf("create %d: %v", i, err)
		}
	}
	list, _ := b.ListSandboxes(ctx, "t")
	if len(list) != 40 {
		t.Fatalf("the second store sees %d of 40 sandboxes", len(list))
	}
}

func TestSQLiteHonoursTheContext(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ListSandboxes(ctx, ""); err == nil {
		t.Fatal("a cancelled context did not stop the call")
	}
	if _, err := s.CreateSandbox(ctx, CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64}); err == nil {
		t.Fatal("a cancelled context did not stop a write")
	}
	// The store still works for the next call.
	if _, err := s.ListSandboxes(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}
