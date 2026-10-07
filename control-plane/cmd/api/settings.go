package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/envcfg"
)

// Two settings that had no ASP_ prefix. The old names still work, with a warning.
const (
	EnvListenAddr  = "ASP_LISTEN_ADDR"
	EnvDatabaseURL = "ASP_DATABASE_URL"

	// EnvEgressDefaultAllow says what a tenant without egress rules may reach.
	EnvEgressDefaultAllow = "ASP_EGRESS_DEFAULT_ALLOW"
	// legacyEgressDenyDefault was the same setting, inverted.
	legacyEgressDenyDefault = "ASP_EGRESS_DENY_DEFAULT"
)

// listenAddr is where the API listens ("" for the default).
func listenAddr() string {
	v, _, _ := envcfg.Get(nil, EnvListenAddr, "LISTEN_ADDR")
	return strings.TrimSpace(v)
}

// databaseURL is the Postgres connection string, or sqlite:///path for a SQLite file ("" for
// the memory store).
func databaseURL() string {
	v, _, _ := envcfg.Get(nil, EnvDatabaseURL, "DATABASE_URL")
	return strings.TrimSpace(v)
}

// sqlitePath says whether raw names a SQLite file (sqlite:///var/lib/asp/asp.db, or
// sqlite:relative.db) and which. A sqlite: URL with no path is an error.
func sqlitePath(raw string) (path string, ok bool, err error) {
	rest, found := strings.CutPrefix(strings.TrimSpace(raw), "sqlite:")
	if !found {
		return "", false, nil
	}
	// sqlite:///abs/path → /abs/path; sqlite://rel.db and sqlite:rel.db → rel.db
	rest = strings.TrimPrefix(rest, "//")
	if rest == "" || rest == "/" {
		return "", true, fmt.Errorf("%s=%q has no path: want sqlite:///var/lib/asp/asp.db", EnvDatabaseURL, "sqlite:")
	}
	return rest, true, nil
}

// resolveEgressDefault settles what a tenant with no rules may reach, once, and
// leaves the answer in ASP_EGRESS_DEFAULT_ALLOW for the store to read:
//
//	ASP_EGRESS_DEFAULT_ALLOW   true allows everything, false denies everything;
//	unset                      a development control plane (memory store) allows,
//	                           one with Postgres denies;
//	ASP_EGRESS_DENY_DEFAULT    the old name, with the opposite meaning: it is read
//	                           when the new one is not set, and says so.
func resolveEgressDefault(memory bool) error {
	allow := memory
	if raw, ok := envcfg.Getenv(EnvEgressDefaultAllow); ok {
		v, valid := envcfg.ParseBool(raw)
		if !valid {
			return fmt.Errorf("%s=%q is not a boolean (1, true, yes, on, 0, false, no, off)", EnvEgressDefaultAllow, raw)
		}
		allow = v
		if _, old := envcfg.Getenv(legacyEgressDenyDefault); old {
			envcfg.Warn(legacyEgressDenyDefault + " is ignored: " + EnvEgressDefaultAllow + " is set")
		}
	} else if raw, ok := envcfg.Getenv(legacyEgressDenyDefault); ok {
		v, valid := envcfg.ParseBool(raw)
		if !valid {
			return fmt.Errorf("%s=%q is not a boolean (1, true, yes, on, 0, false, no, off)", legacyEgressDenyDefault, raw)
		}
		envcfg.Warn(legacyEgressDenyDefault + " is deprecated: use " + EnvEgressDefaultAllow + " with the opposite value")
		allow = !v
	}
	value := "0"
	if allow {
		value = "1"
	}
	return os.Setenv(EnvEgressDefaultAllow, value)
}
