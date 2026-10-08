package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey is the advisory lock that makes control planes that start together take turns
// at the schema ("aspMIGR1").
const migrationLockKey int64 = 0x6173704d49475231

// ApplyMigrations applies embedded *.sql files in lexical order, once each.
// Each file is executed statement-by-statement inside a transaction.
// dir is typically "." when FS was built with //go:embed *.sql.
//
// It runs on one connection that holds an advisory lock: the replicas of a deployment start
// at the same moment, and without it each saw the same migration as not yet applied and the
// losers died with a duplicate key (even CREATE TABLE IF NOT EXISTS fails when two run at
// once). The one that waits finds the work done.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, migrations embed.FS, dir string) error {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrations: connect: %w", err)
	}
	// The connection is closed, not given back: that ends the session, and the lock with it, and
	// leaves the settings below out of the pool.
	conn := pc.Hijack()
	defer func() { _ = conn.Close(context.Background()) }()
	// Waiting for another control plane's migration is not a statement that has taken too long.
	if _, err := conn.Exec(ctx, `SET statement_timeout = 0`); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := conn.Exec(ctx, `SET lock_timeout = '10min'`); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("migrations: wait for the other control plane that is migrating: %w", err)
	}
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return fmt.Errorf("read migration dir %q: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		path := name
		if dir != "" && dir != "." {
			path = dir + "/" + name
		}
		body, err := migrations.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		// A migration may take longer than the pool's statement_timeout (an index
		// on a big table); it keeps the lock_timeout, so one that waits for a lock
		// fails instead of hanging the start.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		for i, stmt := range splitSQL(string(body)) {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("apply %s stmt %d: %w", name, i+1, err)
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations(version) VALUES ($1)`, name,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// splitSQL splits on semicolons at the top level (no string awareness beyond
// simple -- line comments). Adequate for our checked-in migration files.
func splitSQL(src string) []string {
	var out []string
	var b strings.Builder
	lines := strings.Split(src, "\n")
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, part := range strings.Split(b.String(), ";") {
		stmt := strings.TrimSpace(part)
		if stmt == "" {
			continue
		}
		out = append(out, stmt)
	}
	return out
}
