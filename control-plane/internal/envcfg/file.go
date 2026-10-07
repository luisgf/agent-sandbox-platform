package envcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A file of settings is the third place a setting can come from, below the flags and
// the environment: /etc/asp/agent.yaml and the drop-ins in /etc/asp/agent.yaml.d/*.yaml,
// merged in name order, the last file that sets a key winning (like k3s's config.yaml.d).
//
// A key is the name of a setting in snake_case: listen_addr for ASP_LISTEN_ADDR, or for
// --listen-addr. A map nests: `egress: {proxy_listen: ":8888"}` is egress_proxy_listen.
// A list is the comma-separated value the variable takes. A null removes a key an
// earlier file set. A key that no setting has is an error, with the nearest name.

// maxFileBytes is the largest file read: settings are small, and a mistaken path (a log)
// should be an error and not a long wait.
const maxFileBytes = 1 << 20

// File is the settings of a base file and its drop-ins, flattened.
type File struct {
	// Paths are the files read, in the order they were merged.
	Paths []string
	// Values maps each key to its value as the text a variable takes.
	Values map[string]string
	// From maps each key to the file that set it last.
	From map[string]string
	// Loose is the files that users other than the owner can read, by path.
	Loose map[string]bool
}

// LoadFile reads base and the files in base+".d". A base file that does not exist is not
// an error unless required (a path the operator named). A drop-in directory that does not
// exist is nothing.
func LoadFile(base string, required bool) (*File, error) {
	f := &File{Values: map[string]string{}, From: map[string]string{}, Loose: map[string]bool{}}
	if base == "" {
		return f, nil
	}
	paths := []string{}
	if _, err := os.Stat(base); err == nil {
		paths = append(paths, base)
	} else if required || !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config file %s: %w", base, err)
	}
	if entries, err := os.ReadDir(base + ".d"); err == nil {
		names := []string{}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || strings.HasPrefix(n, ".") || !(strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml")) {
				continue
			}
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			paths = append(paths, filepath.Join(base+".d", n))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config drop-ins %s.d: %w", base, err)
	}
	for _, p := range paths {
		if err := f.merge(p); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *File) merge(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("config file %s: not a regular file", path)
	}
	if fi.Size() > maxFileBytes {
		return fmt.Errorf("config file %s: %d bytes is too much for settings (the limit is %d)", path, fi.Size(), maxFileBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	flat := map[string]string{}
	removed := map[string]bool{}
	if err := flatten("", doc, flat, removed, path); err != nil {
		return err
	}
	for k := range removed {
		delete(f.Values, k)
		delete(f.From, k)
	}
	for k, v := range flat {
		f.Values[k] = v
		f.From[k] = path
	}
	f.Paths = append(f.Paths, path)
	if fi.Mode().Perm()&0o004 != 0 {
		f.Loose[path] = true
	}
	return nil
}

// Key normalizes the name of a setting in a file: lower case, dashes as underscores.
func Key(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
}

func validKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func flatten(prefix string, v any, out map[string]string, removed map[string]bool, path string) error {
	switch x := v.(type) {
	case nil:
		if prefix != "" {
			removed[prefix] = true
		}
	case map[string]any:
		for k, e := range x {
			key := Key(k)
			if !validKey(key) {
				return fmt.Errorf("config file %s: %q is not a setting name (letters, digits and underscores)", path, k)
			}
			full := key
			if prefix != "" {
				full = prefix + "_" + key
			}
			if _, dup := out[full]; dup || removed[full] {
				return fmt.Errorf("config file %s: %s is set twice (as a nested key and as a flat one?)", path, full)
			}
			if err := flatten(full, e, out, removed, path); err != nil {
				return err
			}
		}
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := scalar(e)
			if !ok {
				return fmt.Errorf("config file %s: %s is a list of something that is not a plain value", path, prefix)
			}
			parts = append(parts, s)
		}
		if prefix == "" {
			return fmt.Errorf("config file %s: the top level must be a map of settings", path)
		}
		out[prefix] = strings.Join(parts, ",")
	default:
		s, ok := scalar(v)
		if !ok {
			return fmt.Errorf("config file %s: %s has a value of an unsupported kind", path, prefix)
		}
		if prefix == "" {
			return fmt.Errorf("config file %s: the top level must be a map of settings", path)
		}
		out[prefix] = s
	}
	return nil
}

func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	}
	return "", false
}

// Check fails when the file sets a key that is not in known (key → variable). The error
// names every such key, with the file and the nearest setting.
func (f *File) Check(known map[string]string) error {
	var bad []string
	for k := range f.Values {
		if _, ok := known[k]; !ok {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	names := make([]string, 0, len(known))
	for k := range known {
		names = append(names, k)
	}
	var msgs []string
	for _, k := range bad {
		m := fmt.Sprintf("%s: %q is not a setting", f.From[k], k)
		if s := Suggest(k, names); s != "" {
			m += " (did you mean " + s + "?)"
		}
		msgs = append(msgs, m)
	}
	return fmt.Errorf("unknown settings in the config file:\n  %s", strings.Join(msgs, "\n  "))
}

// Suggest returns the name closest to key, or "" when none is close. A key that is the
// name of a variable ("asp_listen_addr") is told apart from the one it names.
func Suggest(key string, names []string) string {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	if rest := strings.TrimPrefix(key, "asp_"); rest != key && set[rest] {
		return rest
	}
	best, bestD := "", 1<<30
	for _, n := range names {
		if d := editDistance(key, n); d < bestD || (d == bestD && n < best) {
			best, bestD = n, d
		}
	}
	limit := len(key) / 3
	if limit < 2 {
		limit = 2
	}
	if bestD <= limit {
		return best
	}
	return ""
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Lookup reads the file's values by variable name; known maps key → variable.
func (f *File) Lookup(known map[string]string) Lookup {
	byVar := make(map[string]string, len(f.Values))
	for k, v := range f.Values {
		if name, ok := known[k]; ok && strings.TrimSpace(v) != "" {
			byVar[name] = v
		}
	}
	return func(name string) (string, bool) {
		v, ok := byVar[name]
		return v, ok
	}
}

// Layer reads the first of the lookups that has the name: the environment, then a file.
func Layer(lookups ...Lookup) Lookup {
	return func(name string) (string, bool) {
		for _, l := range lookups {
			if l == nil {
				continue
			}
			if v, ok := l(name); ok {
				return v, true
			}
		}
		return "", false
	}
}

// ApplyToEnv sets, for each key of the file, the variable it maps to, unless the
// environment already has it (the environment wins over the file). It returns the
// variables it set, with the file each came from. For the binaries that read their
// settings from the environment where they use them.
func (f *File) ApplyToEnv(known map[string]string) (set map[string]string, err error) {
	if err := f.Check(known); err != nil {
		return nil, err
	}
	set = map[string]string{}
	keys := make([]string, 0, len(f.Values))
	for k := range f.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := known[k]
		if _, has := Getenv(name); has {
			continue
		}
		if strings.TrimSpace(f.Values[k]) == "" {
			continue
		}
		if err := os.Setenv(name, f.Values[k]); err != nil {
			return set, err
		}
		set[name] = f.From[k]
	}
	return set, nil
}

// KeyOf is the key a variable has in a file: ASP_LISTEN_ADDR is listen_addr.
func KeyOf(variable string) string { return Key(strings.TrimPrefix(variable, "ASP_")) }
