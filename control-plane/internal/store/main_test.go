package store

import (
	"fmt"
	"os"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pgtest"
)

// testDatabaseURL is the database this test binary created for itself on the
// server DATABASE_URL names; empty when there is none, and the Postgres tests skip.
var testDatabaseURL string

func TestMain(m *testing.M) {
	if pgtest.Server() == "" {
		os.Exit(m.Run())
	}
	url, drop, err := pgtest.Database("asp_store_test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDatabaseURL = url
	code := m.Run()
	drop()
	os.Exit(code)
}
