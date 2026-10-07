// Package settings declares what an operator can configure in a binary: each
// setting is a flag and an environment variable that sets it, with one rule for
// both:
//
//   - the flag wins over the environment, which wins over the default;
//   - the environment variable is ASP_ plus the flag name in capitals with
//     underscores (--control-plane-url is ASP_CONTROL_PLANE_URL), unless the
//     setting says otherwise (Env), and it is always ASP_-prefixed;
//   - booleans are 1/true/yes/on and 0/false/no/off, on the command line and in
//     the environment alike, and a value that is not one is an error, not a
//     setting that silently does nothing;
//   - a name that was renamed keeps working, with a warning (Legacy, LegacyFlag),
//     for as long as it is declared;
//   - the environment is read after the flags are parsed, so the usage text shows
//     defaults and never a value (a token) that came from the environment.
//
// The list of settings is what generated documentation and the tests that keep
// names consistent read (Settings).
package settings

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/envcfg"
)

// Setting describes one setting.
type Setting struct {
	// Flag is the flag name, without dashes.
	Flag string
	// Env is the environment variable, "" for a setting that only has a flag.
	Env string
	// LegacyEnv and LegacyFlags are names that were renamed and still work.
	LegacyEnv   []string
	LegacyFlags []string
	// Kind is string, bool, tri-state (a bool that remembers whether it was set),
	// int, uint, duration or value (anything else a flag.Value can parse).
	Kind    string
	Default string
	Usage   string
	// Secret: the value is a credential, shown as <redacted> when the configuration is printed.
	Secret bool

	// fromEnv sets the variable from an environment value.
	fromEnv func(raw string) error
}

// Set is the settings of a binary.
type Set struct {
	fs   *flag.FlagSet
	look envcfg.Lookup

	list   []*Setting
	byFlag map[string]*Setting
}

// New starts a set on fs. look reads the environment (nil is os.LookupEnv, with
// an empty value counting as unset).
func New(fs *flag.FlagSet, look envcfg.Lookup) *Set {
	return &Set{fs: fs, look: look, byFlag: map[string]*Setting{}}
}

// Option changes how a setting is declared.
type Option func(*Setting)

// Env names the variable when it is not the one the flag name gives. Use it only
// for a name that is already in use and documented, or shared with another binary.
func Env(name string) Option { return func(s *Setting) { s.Env = name } }

// Legacy names variables that were renamed: they are still read, with a warning,
// when the variable of the setting is not set.
func Legacy(names ...string) Option {
	return func(s *Setting) { s.LegacyEnv = append(s.LegacyEnv, names...) }
}

// LegacyFlag names flags that were renamed: they still work, with a warning.
func LegacyFlag(names ...string) Option {
	return func(s *Setting) { s.LegacyFlags = append(s.LegacyFlags, names...) }
}

// Secret marks a setting whose value is a credential.
func Secret() Option { return func(s *Setting) { s.Secret = true } }

// NoEnv makes a setting flag-only: an action (--reap-only), not a configuration.
func NoEnv() Option { return func(s *Setting) { s.Env = "-" } }

// EnvName is the variable a flag name gives: --control-plane-url is
// ASP_CONTROL_PLANE_URL.
func EnvName(flagName string) string {
	return "ASP_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(flagName))
}

func (s *Set) declare(flagName, kind, def, usage string, opts []Option) *Setting {
	st := &Setting{Flag: flagName, Kind: kind, Default: def}
	for _, o := range opts {
		o(st)
	}
	switch st.Env {
	case "":
		st.Env = EnvName(flagName)
	case "-":
		st.Env = ""
	}
	if st.Env != "" {
		usage += " (env " + st.Env + ")"
	}
	st.Usage = usage
	s.list = append(s.list, st)
	s.byFlag[flagName] = st
	return st
}

// legacyFlags registers the old flag names of st as aliases of its flag.
func (s *Set) legacyFlags(st *Setting) {
	target := s.fs.Lookup(st.Flag)
	for _, old := range st.LegacyFlags {
		s.byFlag[old] = st
		s.fs.Var(&alias{old: old, now: st.Flag, target: target.Value}, old, "deprecated: use --"+st.Flag)
	}
}

// alias is a renamed flag: it sets the flag that replaced it and says so.
type alias struct {
	old, now string
	target   flag.Value
}

func (a *alias) String() string {
	if a == nil || a.target == nil {
		return ""
	}
	return a.target.String()
}

func (a *alias) Set(v string) error {
	envcfg.Warn("--" + a.old + " is deprecated: use --" + a.now)
	return a.target.Set(v)
}

func (a *alias) IsBoolFlag() bool {
	b, ok := a.target.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// String declares a string setting.
func (s *Set) String(p *string, flagName, def, usage string, opts ...Option) {
	st := s.declare(flagName, "string", def, usage, opts)
	st.fromEnv = func(raw string) error { *p = raw; return nil }
	s.fs.StringVar(p, flagName, def, st.Usage)
	s.legacyFlags(st)
}

// Int declares an integer setting.
func (s *Set) Int(p *int, flagName string, def int, usage string, opts ...Option) {
	st := s.declare(flagName, "int", strconv.Itoa(def), usage, opts)
	st.fromEnv = func(raw string) error {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return errors.New("not an integer")
		}
		*p = n
		return nil
	}
	s.fs.IntVar(p, flagName, def, st.Usage)
	s.legacyFlags(st)
}

// Uint declares an unsigned integer setting.
func (s *Set) Uint(p *uint, flagName string, def uint, usage string, opts ...Option) {
	st := s.declare(flagName, "uint", strconv.FormatUint(uint64(def), 10), usage, opts)
	st.fromEnv = func(raw string) error {
		n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return errors.New("not an unsigned integer")
		}
		*p = uint(n)
		return nil
	}
	s.fs.UintVar(p, flagName, def, st.Usage)
	s.legacyFlags(st)
}

// Duration declares a duration setting (Go syntax: 90s, 5m).
func (s *Set) Duration(p *time.Duration, flagName string, def time.Duration, usage string, opts ...Option) {
	st := s.declare(flagName, "duration", def.String(), usage, opts)
	st.fromEnv = func(raw string) error {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return errors.New("not a duration such as 30s or 5m")
		}
		*p = d
		return nil
	}
	s.fs.DurationVar(p, flagName, def, st.Usage)
	s.legacyFlags(st)
}

// boolValue is a bool flag that understands every spelling of envcfg.ParseBool.
type boolValue struct{ p *bool }

func (b boolValue) String() string {
	if b.p == nil {
		return "false"
	}
	return strconv.FormatBool(*b.p)
}

func (b boolValue) Set(v string) error {
	parsed, ok := envcfg.ParseBool(v)
	if !ok {
		return fmt.Errorf("%q is not a boolean (1, true, yes, on, 0, false, no, off)", v)
	}
	*b.p = parsed
	return nil
}

func (b boolValue) IsBoolFlag() bool { return true }

// Bool declares a boolean setting.
func (s *Set) Bool(p *bool, flagName string, def bool, usage string, opts ...Option) {
	st := s.declare(flagName, "bool", strconv.FormatBool(def), usage, opts)
	*p = def
	st.fromEnv = func(raw string) error { return boolValue{p}.Set(raw) }
	s.fs.Var(boolValue{p}, flagName, st.Usage)
	s.legacyFlags(st)
}

// Tri declares a setting whose value is a flag.Value that can be bare
// (IsBoolFlag) and that parses the same spellings from the environment: use it for
// a boolean that must tell "not set" from false.
func (s *Set) Tri(v flag.Value, flagName, usage string, opts ...Option) {
	st := s.declare(flagName, "tri-state", v.String(), usage, opts)
	st.fromEnv = v.Set
	s.fs.Var(v, flagName, st.Usage)
	s.legacyFlags(st)
}

// noop is a flag that does nothing any more.
type noop struct{ name, why string }

func (n noop) String() string { return "" }
func (n noop) Set(string) error {
	envcfg.Warn("--" + n.name + " has no effect: " + n.why)
	return nil
}
func (n noop) IsBoolFlag() bool { return true }

// Deprecated declares a flag, and the variable envVar ("" for none), that no longer
// do anything. They are still accepted, so a unit file that sets them keeps working,
// and say that they have no effect and why.
func (s *Set) Deprecated(flagName, envVar, why string) {
	usage := "deprecated, no effect: " + why
	if envVar != "" {
		usage += " (env " + envVar + ")"
	}
	st := &Setting{Flag: flagName, Env: envVar, Kind: "deprecated", Usage: usage}
	s.list = append(s.list, st)
	s.byFlag[flagName] = st
	if envVar != "" {
		st.fromEnv = func(string) error {
			envcfg.Warn(envVar + " has no effect: " + why)
			return nil
		}
	}
	s.fs.Var(noop{flagName, why}, flagName, usage)
}

// Settings lists what was declared, in order.
func (s *Set) Settings() []Setting {
	out := make([]Setting, len(s.list))
	for i, st := range s.list {
		out[i] = *st
	}
	return out
}

// Fallback makes the settings read from lookup when the environment has no value: a
// configuration file sits below the environment and above the defaults.
func (s *Set) Fallback(lookup envcfg.Lookup) {
	env := s.look
	if env == nil {
		env = envcfg.Getenv
	}
	s.look = envcfg.Layer(env, lookup)
}

// Flags is the flag set the settings are declared on.
func (s *Set) Flags() *flag.FlagSet { return s.fs }

// Parse parses args, then applies the environment to every setting no flag set.
// The error lists every value of the environment that is not valid, by variable.
func (s *Set) Parse(args []string) error {
	if err := s.fs.Parse(args); err != nil {
		return err
	}
	return s.Apply()
}

// Apply reads the environment for the settings whose flag was not given. Parse
// calls it; it is separate for a caller that parses by itself.
func (s *Set) Apply() error {
	given := map[*Setting]bool{}
	s.fs.Visit(func(f *flag.Flag) {
		if st := s.byFlag[f.Name]; st != nil {
			given[st] = true
		}
	})
	var errs []error
	for _, st := range s.list {
		if given[st] || st.Env == "" || st.fromEnv == nil {
			continue
		}
		raw, from, ok := envcfg.Get(s.look, st.Env, st.LegacyEnv...)
		if !ok {
			continue
		}
		if err := st.fromEnv(raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", from, err))
		}
	}
	return errors.Join(errs...)
}
