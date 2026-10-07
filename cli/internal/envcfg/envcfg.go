// Package envcfg reads configuration from the environment the same way in every
// ASP binary: one spelling for booleans, one way to rename a variable without
// breaking the people who set the old one.
//
// The same file is in the node-agent, the control plane and the CLI (they are
// separate modules); a test in the node-agent fails when the copies differ.
package envcfg

import (
	"log/slog"
	"os"
	"strings"
	"sync"
)

// ParseBool understands 1, true, yes and on, and 0, false, no and off, in any case
// and with spaces around. ok is false for anything else, the empty string included.
func ParseBool(s string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

// Truthy reports whether the variable is set to a true spelling. Unset, false and
// anything that is not a boolean are false.
func Truthy(name string) bool {
	v, ok := ParseBool(os.Getenv(name))
	return ok && v
}

// Warn is how a deprecated name is reported. A binary that does not want a log
// line (the CLI) replaces it.
var Warn = func(msg string) { slog.Warn(msg) }

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

// warnOnce says msg the first time key is seen in this process.
func warnOnce(key, msg string) {
	warnedMu.Lock()
	first := !warned[key]
	warned[key] = true
	warnedMu.Unlock()
	if first {
		Warn(msg)
	}
}

// ResetWarnings forgets which deprecated names were already reported (tests).
func ResetWarnings() {
	warnedMu.Lock()
	warned = map[string]bool{}
	warnedMu.Unlock()
}

// Lookup reads variables; tests and the settings of the node-agent substitute their own.
type Lookup func(name string) (string, bool)

// Getenv is os.LookupEnv where an empty value counts as not set.
func Getenv(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return v, true
}

// Get returns the value of name and the name it was read from. When name is not
// set it looks for each of the legacy names, in order, and says (once) that the
// name is deprecated. When both are set, name wins and the legacy one is reported
// as ignored if it disagrees.
func Get(look Lookup, name string, legacy ...string) (value, from string, ok bool) {
	if look == nil {
		look = Getenv
	}
	if v, set := look(name); set {
		for _, old := range legacy {
			if ov, oset := look(old); oset && ov != v {
				warnOnce("ignored:"+old, old+" is ignored: "+name+" is set to another value")
			}
		}
		return v, name, true
	}
	for _, old := range legacy {
		if v, set := look(old); set {
			warnOnce("deprecated:"+old, old+" is deprecated: use "+name)
			return v, old, true
		}
	}
	return "", "", false
}
