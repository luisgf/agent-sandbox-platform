// Package pgtest lets test binaries share one Postgres server without sharing its
// tables. Each binary creates a database of its own on the server DATABASE_URL
// names and drops it when its tests end: go test runs packages in parallel, and two
// of them truncating the same tables fail each other's tests at random.
//
// It is for tests only and imports nothing of the control plane, so the packages
// whose tests use it (store, api) can both import it.
package pgtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvURL names the server. The role in it needs CREATEDB.
const EnvURL = "DATABASE_URL"

// staleAfter is how old a database of ours must be before another run drops it: it
// belongs to a test binary that was killed before it could clean up.
const staleAfter = 6 * time.Hour

// Server returns the connection string of the server the tests may use, "" when
// DATABASE_URL is not set (the Postgres tests then skip).
func Server() string { return strings.TrimSpace(os.Getenv(EnvURL)) }

// Database creates a database on the server for this test binary and returns its
// connection string and the function that drops it. prefix names the owner
// ("asp_api_test"); it must be lower-case letters, digits and underscores. Only
// URL-style connection strings (postgres://...) are supported.
func Database(prefix string) (dbURL string, drop func(), err error) {
	base := Server()
	if base == "" {
		return "", nil, errors.New("pgtest: " + EnvURL + " is not set")
	}
	if !validName(prefix) {
		return "", nil, fmt.Errorf("pgtest: bad database prefix %q", prefix)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", nil, fmt.Errorf("pgtest: %s must be a postgres:// URL", EnvURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return "", nil, fmt.Errorf("pgtest: connect to %s: %w", u.Redacted(), err)
	}
	defer conn.Close(ctx)

	dropStale(ctx, conn, prefix)
	name := fmt.Sprintf("%s_%d_%d", prefix, time.Now().Unix(), os.Getpid())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
		return "", nil, fmt.Errorf("pgtest: create database (the role needs CREATEDB): %w", err)
	}
	u.Path = "/" + name
	return u.String(), func() { dropDatabase(base, name) }, nil
}

// dropStale removes databases a killed run of the same owner left behind.
func dropStale(ctx context.Context, conn *pgx.Conn, prefix string) {
	rows, err := conn.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1`, prefix+"\\_%")
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		// prefix_<unix seconds>_<pid>
		parts := strings.Split(strings.TrimPrefix(name, prefix+"_"), "_")
		if len(parts) != 2 {
			continue
		}
		secs, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || time.Since(time.Unix(secs, 0)) < staleAfter {
			continue
		}
		stale = append(stale, name)
	}
	rows.Close()
	for _, name := range stale {
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
	}
}

func dropDatabase(serverURL, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, serverURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: drop %s: %v\n", name, err)
		return
	}
	defer conn.Close(ctx)
	// FORCE: a pool a test forgot to close must not keep the database alive.
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)"); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: drop %s: %v\n", name, err)
	}
}

func quote(name string) string { return pgx.Identifier{name}.Sanitize() }

func validName(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
