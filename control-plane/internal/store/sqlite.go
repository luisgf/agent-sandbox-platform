package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
	"github.com/luisgf/agent-sandbox-platform/control-plane/migrations"
)

// SQLiteStore implements Store on one SQLite file (modernc.org/sqlite, no CGO). It is the
// store of a single host: the control plane and its database in one place, no server to run.
// Postgres stays the store for more than one control plane.
//
// It follows the Postgres store statement by statement, so the contract is the same one
// (parity_test.go plays the same script against it). What differs is how SQLite works:
//   - one connection: every call is serialised, which is what the advisory lock and the
//     FOR UPDATE of Postgres are for. A method that holds a transaction must read through it,
//     or it would wait for its own connection;
//   - RETURNING gives the new row, not the old one, so the previous state is read first;
//   - times are TEXT of one fixed width (see sqlTime), arrays and JSON are TEXT, booleans 0/1.
type SQLiteStore struct {
	db              *sql.DB
	provisionNodeID string
	schedCfg        sched.Config
}

// OpenSQLite opens the database at path, creating the file, and brings its schema up to
// date. The directory has to exist.
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sqlite: no path")
	}
	// The file holds key hashes and fence tokens: private from its first byte. SQLite creates a
	// missing file with the mode its umask leaves, so the file is created here, empty (which
	// SQLite takes for a new database), and its -wal and -shm follow its mode.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		_ = f.Close()
	} else {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	// WAL: a reader never waits for the writer and the writer never waits for fsync of its
	// readers; NORMAL syncs at checkpoints, so a power cut can lose the last commits but not
	// corrupt the file. foreign_keys is a per-connection setting. A transaction takes the
	// write lock when it begins (immediate), so two processes on one file queue instead of
	// failing when one of them tries to upgrade a read to a write.
	dsn := "file:" + escapeSQLitePath(path) +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &SQLiteStore{db: db, provisionNodeID: DefaultLocalNodeID, schedCfg: sched.DefaultConfig()}
	if err := s.applyMigrations(ctx, migrations.SQLiteFS, "sqlite"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// escapeSQLitePath makes a file name safe inside a "file:" URI.
func escapeSQLitePath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
}

// Close closes the database.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// DB is the database, for the tests and for the health check.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// SetSchedConfig sets the placement policy (ASP_SCHED_POLICY and friends).
func (s *SQLiteStore) SetSchedConfig(cfg sched.Config) { s.schedCfg = cfg }

// SetProvisionNodeID names the node the stub provisioner places sandboxes on.
func (s *SQLiteStore) SetProvisionNodeID(id string) { s.provisionNodeID = id }

// applyMigrations applies the embedded *.sql files of dir in name order, once each, each in
// a transaction with the line that records it.
func (s *SQLiteStore) applyMigrations(ctx context.Context, migrations embed.FS, dir string) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return fmt.Errorf("read migration dir %q: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version=?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrations.ReadFile(dir + "/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			for i, stmt := range splitSQL(string(body)) {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply %s stmt %d: %w", name, i+1, err)
				}
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?,?)`, name, sqlTime(time.Now()))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// tx runs fn in a transaction: committed if it returns nil, rolled back otherwise.
func (s *SQLiteStore) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // after a commit, a no-op
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// runner is what *sql.DB and *sql.Tx have in common: the helpers take one, so the same
// code reads through a transaction or outside one.
type runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func run(ctx context.Context, q runner, query string, args ...any) (sql.Result, error) {
	return q.ExecContext(ctx, query, sqlArgs(args)...)
}

func rowsOf(ctx context.Context, q runner, query string, args ...any) (*sql.Rows, error) {
	return q.QueryContext(ctx, query, sqlArgs(args)...)
}

func rowOf(ctx context.Context, q runner, query string, args ...any) *sql.Row {
	return q.QueryRowContext(ctx, query, sqlArgs(args)...)
}

func affected(res sql.Result) int64 {
	n, _ := res.RowsAffected()
	return n
}

// SQLiteTime is how SQLiteStore stores a time; the tests that set a column by hand use it.
func SQLiteTime(t time.Time) string { return sqlTime(t) }

// sqlTime is how a time is stored: UTC, microseconds, always the same width, so that
// comparing the text compares the instants.
func sqlTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000Z") }

// sqlArgs turns the values the store passes into what SQLite stores: times as sqlTime,
// booleans as 0/1, lists and JSON as text.
func sqlArgs(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case time.Time:
			out[i] = sqlTime(v)
		case *time.Time:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = sqlTime(*v)
			}
		case bool:
			if v {
				out[i] = int64(1)
			} else {
				out[i] = int64(0)
			}
		case []string:
			b, _ := json.Marshal(v)
			out[i] = string(b)
		case json.RawMessage:
			out[i] = string(v)
		case []byte:
			out[i] = string(v)
		case *string:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = *v
			}
		case *int:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = int64(*v)
			}
		case *int64:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = *v
			}
		default:
			out[i] = a
		}
	}
	return out
}

// inList is "(?,?,…)" for n values.
func inList(n int) string {
	if n <= 0 {
		return "(NULL)"
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

// strArgs converts a list for the variadic args of a query.
func strArgs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// tsCol scans a stored time into a time.Time.
type tsCol struct{ dst *time.Time }

func (c tsCol) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return c.parse(v)
	case []byte:
		return c.parse(string(v))
	case time.Time:
		*c.dst = v.UTC()
		return nil
	case nil:
		*c.dst = time.Time{}
		return nil
	}
	return fmt.Errorf("sqlite: cannot read a time from %T", src)
}

func (c tsCol) parse(s string) error {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("sqlite: bad time %q: %w", s, err)
	}
	*c.dst = t.UTC()
	return nil
}

// tsPtrCol scans a stored, nullable time into a *time.Time.
type tsPtrCol struct{ dst **time.Time }

func (c tsPtrCol) Scan(src any) error {
	if src == nil {
		*c.dst = nil
		return nil
	}
	var t time.Time
	if err := (tsCol{&t}).Scan(src); err != nil {
		return err
	}
	*c.dst = &t
	return nil
}

// jsonStrings scans a JSON array of strings.
type jsonStrings struct{ dst *[]string }

func (c jsonStrings) Scan(src any) error {
	var raw string
	switch v := src.(type) {
	case nil:
		*c.dst = nil
		return nil
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("sqlite: cannot read a list from %T", src)
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fmt.Errorf("sqlite: bad list %q: %w", raw, err)
	}
	*c.dst = out
	return nil
}

// rawJSON scans JSON text into a json.RawMessage.
type rawJSON struct{ dst *json.RawMessage }

func (c rawJSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*c.dst = nil
	case string:
		*c.dst = json.RawMessage(v)
	case []byte:
		*c.dst = append(json.RawMessage(nil), v...)
	default:
		return fmt.Errorf("sqlite: cannot read JSON from %T", src)
	}
	return nil
}

// scanSandboxSQL reads sandboxColumns; pre are the destinations of columns a query selects
// before them (the previous state).
func scanSandboxSQL(row scannable, pre ...any) (Sandbox, error) {
	var sb Sandbox
	var state string
	err := row.Scan(append(pre,
		&sb.ID, &sb.TenantID, &sb.NodeID, &state, &sb.VMMProfile, &sb.ImageRef,
		&sb.CPUMillis, &sb.MemoryMiB, &sb.StateVersion,
		tsCol{&sb.CreatedAt}, tsCol{&sb.UpdatedAt}, &sb.OwnerSub, &sb.OwnerEmail, tsCol{&sb.LastActivityAt}, &sb.StopReason,
		&sb.WorkspaceHostPath,
		&sb.LocalNet, &sb.LocalNetState, tsPtrCol{&sb.LocalNetAttachedAt}, tsPtrCol{&sb.LocalNetGrantExpiresAt},
		&sb.LocalNetGrantHash, &sb.LocalNetClientPublic, &sb.LocalNetNodePublic,
		&sb.LocalNetListenPort, &sb.LocalNetNodeAddr, &sb.LocalNetClientAddr,
		&sb.StatusDetail, &sb.BootCount, tsPtrCol{&sb.StoppedAt}, tsPtrCol{&sb.BootedAt},
	)...)
	if err != nil {
		return Sandbox{}, err
	}
	sb.State = SandboxState(state)
	return sb, nil
}

// scanNodeSQL reads nodeColumns.
func scanNodeSQL(row scannable, pre ...any) (Node, error) {
	var n Node
	err := row.Scan(append(pre,
		&n.ID, &n.Name, &n.Endpoint, &n.AgentEndpoint, &n.State, jsonStrings{&n.VMMProfiles},
		&n.CapacityCPU, &n.CapacityMemMiB, &n.MaxSandboxes, &n.Cordoned, &n.AcceptsWork, &n.LocalNetDial, &n.AgentInstanceID,
		&n.CertFingerprint, &n.CertSerial, tsPtrCol{&n.CertNotAfter}, &n.FenceToken, &n.FenceEndpoint, tsPtrCol{&n.EnrolledAt},
		tsPtrCol{&n.RevokedAt}, tsPtrCol{&n.LastSeenAt}, tsCol{&n.CreatedAt}, tsCol{&n.UpdatedAt}, &n.DiskFreeMiB, &n.EgressEnforced, &n.AgentVersion,
		&n.GuestKernelDigest, &n.GuestImageDigest,
	)...)
	if err != nil {
		return Node{}, err
	}
	if n.VMMProfiles == nil {
		n.VMMProfiles = []string{}
	}
	return n, nil
}

// isNoRows reports whether err is "the query returned no row".
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// sqliteConstraint reports whether err is a SQLite constraint violation of the given
// extended code, and its text (which names the columns).
func sqliteConstraint(err error, code int) (string, bool) {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == code {
		return se.Error(), true
	}
	return "", false
}

// uniqueViolation reports whether err is a UNIQUE (or primary key) violation and, if it
// names columns, whether the text mentions every one of them ("api_keys.key_prefix").
func uniqueViolation(err error, columns ...string) bool {
	msg, ok := sqliteConstraint(err, sqlite3.SQLITE_CONSTRAINT_UNIQUE)
	if !ok {
		msg, ok = sqliteConstraint(err, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
	}
	if !ok {
		return false
	}
	for _, c := range columns {
		if !strings.Contains(msg, c) {
			return false
		}
	}
	return true
}

func sqliteNodeWriteError(err error, name string) error {
	if uniqueViolation(err, "nodes.name") {
		return errNodeNameTaken(name)
	}
	return err
}

func sqliteAPIKeyWriteError(err error, tenantID, name, prefix string) error {
	switch {
	case uniqueViolation(err, "api_keys.tenant_id", "api_keys.name"):
		return errKeyNameTaken(tenantID, name)
	case uniqueViolation(err, "api_keys.key_prefix"):
		return errKeyPrefixTaken(prefix)
	case uniqueViolation(err, "api_keys.secret_hash"):
		return errKeySecretTaken()
	}
	return err
}

// emitEventSQL appends a sandbox event. An event of a sandbox that does not exist is
// ErrNotFound, as in the other stores.
func emitEventSQL(ctx context.Context, q runner, input EmitEventInput) error {
	if strings.TrimSpace(input.SandboxID) == "" || strings.TrimSpace(input.EventType) == "" {
		return fmt.Errorf("%w: sandbox_id and event_type required", ErrInvalidInput)
	}
	actor := input.Actor
	if actor == "" {
		actor = "system"
	}
	payload := input.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	_, err := run(ctx, q, `
		INSERT INTO sandbox_events (
			sandbox_id, tenant_id, event_type, from_state, to_state,
			actor, actor_sub, request_id, payload, created_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		input.SandboxID, input.TenantID, input.EventType,
		input.FromState, input.ToState, actor, strings.TrimSpace(input.ActorSub),
		input.RequestID, payload, time.Now().UTC(),
	)
	if _, ok := sqliteConstraint(err, sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY); ok {
		// The tenant has to exist too, but a sandbox does not exist without one: this is the sandbox.
		var n int
		if rowOf(ctx, q, `SELECT count(*) FROM sandboxes WHERE id=?`, input.SandboxID).Scan(&n) == nil && n == 0 {
			return ErrNotFound
		}
	}
	return err
}

// nodeEventSQL appends a node event.
func nodeEventSQL(ctx context.Context, q runner, nodeID, eventType, actor string, payload any) error {
	_, err := run(ctx, q, `
		INSERT INTO node_events (node_id, event_type, actor, payload, created_at)
		VALUES (?,?,?,?,?)`, nodeID, eventType, actor, mustJSON(payload), time.Now().UTC())
	return err
}

func (s *SQLiteStore) ensureTenant(ctx context.Context, q runner, id string) error {
	now := time.Now().UTC()
	_, err := run(ctx, q, `
		INSERT INTO tenants (id, name, created_at, updated_at) VALUES (?,?,?,?)
		ON CONFLICT (id) DO NOTHING`, id, id, now, now)
	return err
}

// EnsureBootstrapNode upserts the stub provision node so FK assigns succeed.
func (s *SQLiteStore) EnsureBootstrapNode(ctx context.Context) error {
	_, err := s.RegisterNode(ctx, RegisterNodeInput{
		ID:       s.provisionNodeID,
		Name:     s.provisionNodeID,
		Endpoint: "local://stub",
	})
	return err
}

// placementCandidatesSQL reads nodes and their usage through q, for the scheduler.
func placementCandidatesSQL(ctx context.Context, q runner) ([]sched.Candidate, error) {
	usage, err := nodeUsageSQL(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := rowsOf(ctx, q, `SELECT `+nodeColumns+` FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sched.Candidate
	for rows.Next() {
		n, err := scanNodeSQL(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate(n, usage[n.ID]))
	}
	return out, rows.Err()
}

func nodeUsageSQL(ctx context.Context, q runner) (map[string]NodeUsage, error) {
	states := occupyingStateNames()
	rows, err := rowsOf(ctx, q, `
		SELECT node_id, COALESCE(SUM(cpu_millis),0), COALESCE(SUM(memory_mib),0), COUNT(*)
		FROM sandboxes WHERE node_id IS NOT NULL AND state IN `+inList(len(states))+`
		GROUP BY node_id`, strArgs(states)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := map[string]NodeUsage{}
	for rows.Next() {
		var id string
		var u NodeUsage
		if err := rows.Scan(&id, &u.CPUMillis, &u.MemoryMiB, &u.Sandboxes); err != nil {
			return nil, err
		}
		usage[id] = u
	}
	return usage, rows.Err()
}

var _ Store = (*SQLiteStore)(nil)
