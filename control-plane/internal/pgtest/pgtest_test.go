package pgtest

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDatabaseNeedsAServerAndAGoodPrefix(t *testing.T) {
	t.Setenv(EnvURL, "")
	if _, _, err := Database("asp_x"); err == nil {
		t.Fatal("with no DATABASE_URL, Database must fail")
	}
	t.Setenv(EnvURL, "postgres://u:p@127.0.0.1:1/db")
	for _, prefix := range []string{"", "Upper", "has space", "semi;colon", strings.Repeat("a", 41)} {
		if _, _, err := Database(prefix); err == nil || !strings.Contains(err.Error(), "prefix") {
			t.Errorf("prefix %q: want a prefix error, got %v", prefix, err)
		}
	}
	t.Setenv(EnvURL, "host=127.0.0.1 dbname=asp")
	if _, _, err := Database("asp_x"); err == nil || !strings.Contains(err.Error(), "postgres://") {
		t.Errorf("a key=value connection string: want a refusal that says what is accepted, got %v", err)
	}
}

func exists(t *testing.T, conn *pgx.Conn, name string) bool {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_database WHERE datname=$1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A database is made for the binary, usable, and gone when dropped; the ones a killed
// binary left are dropped by the next, but not those of a binary that is still running.
func TestDatabaseLifecycle(t *testing.T) {
	server := Server()
	if server == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, server)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	const prefix = "asp_pgtest_check"
	old := fmt.Sprintf("%s_%d_1", prefix, time.Now().Add(-2*staleAfter).Unix())
	recent := fmt.Sprintf("%s_%d_2", prefix, time.Now().Add(-time.Minute).Unix())
	other := fmt.Sprintf("asp_pgtest_other_%d_3", time.Now().Add(-2*staleAfter).Unix())
	for _, name := range []string{old, recent, other} {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+quote(name)); err != nil {
			t.Fatal(err)
		}
		defer admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name)+" WITH (FORCE)")
	}

	url, drop, err := Database(prefix)
	if err != nil {
		t.Fatal(err)
	}
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !strings.HasPrefix(name, prefix+"_") || !strings.HasSuffix(name, fmt.Sprintf("_%d", os.Getpid())) {
		t.Fatalf("database name %q", name)
	}
	if !exists(t, admin, name) {
		t.Fatal("the database was not created")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("the database is not usable at %s: %v", url, err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE t (x int)`); err != nil {
		t.Fatal(err)
	}
	// An open connection must not keep the database alive.
	drop()
	_ = conn.Close(ctx)
	if exists(t, admin, name) {
		t.Fatal("drop left the database")
	}
	if exists(t, admin, old) {
		t.Error("a database of this owner left by a run long gone was not dropped")
	}
	if !exists(t, admin, recent) {
		t.Error("a database that may belong to a run in progress was dropped")
	}
	if !exists(t, admin, other) {
		t.Error("a database of another owner was dropped")
	}
}
