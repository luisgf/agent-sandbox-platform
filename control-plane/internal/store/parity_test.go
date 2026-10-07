package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The memory store is what the tests and a lab run on; Postgres is what a
// deployment runs on. They are two implementations of one contract (Store), and
// nothing makes them agree except these tests: a handler tested on memory can pass
// and fail on Postgres, or the other way round.
//
// The parity test plays one script against both and requires the same result and
// the same class of error from every call. A script step names the Store method
// it calls, and the test fails if a method of Store has no step, so a new method
// cannot be added without saying how the two stores must agree on it.

// step is one call of the script.
type step struct {
	// method is the Store method the step exercises ("" for a step that only
	// prepares state with other calls).
	method string
	name   string
	// unordered: the contract does not order the result, so it is compared as a set.
	unordered bool
	run       func(w *world) (any, error)
}

// world is one store the script runs against, with the names it gave the ids the
// store chose. Both stores see the same script, but each makes its own ids.
type world struct {
	t     *testing.T
	s     Store
	real  map[string]string // name → id
	named map[string]string // id → name
	memo  map[string]string // values a later step needs (a grant the store made up)
}

func newWorld(t *testing.T, s Store) *world {
	return &world{t: t, s: s, real: map[string]string{}, named: map[string]string{}, memo: map[string]string{}}
}

// bind gives the id a store chose a name, so the two stores' results compare.
func (w *world) bind(name, id string) {
	if id == "" {
		return
	}
	w.real[name] = id
	w.named[id] = name
}

// id is the id bound to name. A name nothing is bound to stands for itself, which
// is how a step asks for an id that does not exist.
func (w *world) id(name string) string {
	if id, ok := w.real[name]; ok {
		return id
	}
	return name
}

// outcome is what a step produced, with ids and clock readings made comparable.
type outcome struct {
	Value any
	Err   string
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

// fieldsNotCompared are values the two stores pick differently and nothing depends on.
var fieldsNotCompared = map[string]bool{
	"SandboxEvent.ID": true, // a sequence; only the order matters
}

// fieldsUnordered are lists whose order follows the ids, which differ between the
// stores: they are compared as sets.
var fieldsUnordered = map[string]bool{
	"NodeWork.Assigned": true,
	"NodeWork.Retained": true,
}

// fieldsNilMatters are lists where nil and empty differ: a node tells a control plane
// that has the list from one that predates it by it. Everywhere else, none is none.
var fieldsNilMatters = map[string]bool{
	"NodeWork.Retained": true,
}

// fieldsOnlyPresence are values a store makes up at random (the hash of a grant): that
// there is one is compared, not which.
var fieldsOnlyPresence = map[string]bool{
	"Sandbox.LocalNetGrantHash": true,
}

// uuid is the shape of the ids the stores make up. One no step named is shown as
// "<uuid>": that the store made one is compared, not which.
var uuid = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// errClass names the sentinel an error wraps: callers branch on it (it picks the HTTP
// status), so the two stores must agree on it.
func errClass(err error) string {
	if err == nil {
		return "ok"
	}
	for _, s := range []struct {
		name string
		err  error
	}{
		{"ErrNotFound", ErrNotFound},
		{"ErrInvalidInput", ErrInvalidInput},
		{"ErrAlreadyExists", ErrAlreadyExists},
		{"ErrUnauthorized", ErrUnauthorized},
		{"ErrConflict", ErrConflict},
		{"ErrEnrollTokenInvalid", ErrEnrollTokenInvalid},
		{"ErrEnrollTokenPinned", ErrEnrollTokenPinned},
		{"ErrNodeEnrolled", ErrNodeEnrolled},
		{"ErrNoCapacity", ErrNoCapacity},
		{"ErrNodeUnavailable", ErrNodeUnavailable},
	} {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	return "unclassified"
}

// text replaces the ids a store chose with their names.
func (w *world) text(s string) string {
	ids := make([]string, 0, len(w.named))
	for id := range w.named {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) })
	for _, id := range ids {
		s = strings.ReplaceAll(s, id, "<"+w.named[id]+">")
	}
	return uuid.ReplaceAllString(s, "<uuid>")
}

func (w *world) outcome(v any, err error) outcome {
	o := outcome{Value: w.norm(reflect.ValueOf(v), "")}
	o.Err = errClass(err)
	if err != nil {
		o.Err += ": " + w.text(err.Error())
	}
	return o
}

// norm turns a result into a tree of maps, slices and scalars that two stores can
// be compared on: ids are names, clock readings are "<time>" (or "<zero>"), json
// is parsed.
func (w *world) norm(v reflect.Value, field string) any {
	if !v.IsValid() {
		return nil
	}
	switch v.Type() {
	case timeType:
		if v.Interface().(time.Time).IsZero() {
			return "<zero>"
		}
		return "<time>"
	case rawType:
		if v.Len() == 0 {
			return "<empty json>"
		}
		var j any
		if err := json.Unmarshal(v.Bytes(), &j); err != nil {
			return "<bad json> " + w.text(string(v.Bytes()))
		}
		return w.normJSON(j)
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return w.norm(v.Elem(), field)
	case reflect.Ptr:
		if v.IsNil() {
			return nil
		}
		return w.norm(v.Elem(), field)
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			key := v.Type().Name() + "." + f.Name
			if fieldsNotCompared[key] {
				continue
			}
			out[f.Name] = w.norm(v.Field(i), key)
		}
		return out
	case reflect.Slice:
		if v.IsNil() && fieldsNilMatters[field] {
			return "<nil slice>"
		}
		fallthrough
	case reflect.Array:
		out := make([]any, 0, v.Len())
		for i := 0; i < v.Len(); i++ {
			out = append(out, w.norm(v.Index(i), field))
		}
		if fieldsUnordered[field] {
			sortAny(out)
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		for _, k := range v.MapKeys() {
			out[w.text(fmt.Sprint(k.Interface()))] = w.norm(v.MapIndex(k), field)
		}
		return out
	case reflect.String:
		if fieldsOnlyPresence[field] {
			if v.String() == "" {
				return "<none>"
			}
			return "<set>"
		}
		return w.text(v.String())
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(v.Uint())
	case reflect.Float32, reflect.Float64:
		return v.Float()
	}
	return fmt.Sprint(v.Interface())
}

func (w *world) normJSON(j any) any {
	switch x := j.(type) {
	case string:
		return w.text(x)
	case []any:
		for i := range x {
			x[i] = w.normJSON(x[i])
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = w.normJSON(e)
		}
		return x
	}
	return j
}

// play runs the script on one store and returns what each step produced.
func play(t *testing.T, s Store, script []step) []outcome {
	w := newWorld(t, s)
	out := make([]outcome, len(script))
	for i, st := range script {
		v, err := st.run(w)
		o := w.outcome(v, err)
		if st.unordered {
			if list, ok := o.Value.([]any); ok {
				sortAny(list)
			}
		}
		out[i] = o
	}
	return out
}

func sortAny(list []any) {
	keys := make([]string, len(list))
	for i, e := range list {
		b, _ := json.Marshal(e)
		keys[i] = string(b)
	}
	sort.Sort(byKey{list, keys})
}

type byKey struct {
	list []any
	keys []string
}

func (b byKey) Len() int           { return len(b.list) }
func (b byKey) Less(i, j int) bool { return b.keys[i] < b.keys[j] }
func (b byKey) Swap(i, j int) {
	b.list[i], b.list[j] = b.list[j], b.list[i]
	b.keys[i], b.keys[j] = b.keys[j], b.keys[i]
}

func pretty(v any) string {
	b, err := json.MarshalIndent(v, "    ", "  ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// compare reports every step where the two stores did not do the same.
func compare(t *testing.T, script []step, nameA string, a []outcome, nameB string, b []outcome) {
	t.Helper()
	diffs := 0
	for i, st := range script {
		if reflect.DeepEqual(a[i], b[i]) {
			continue
		}
		diffs++
		var sb strings.Builder
		fmt.Fprintf(&sb, "step %d, %s %s:\n", i, st.method, st.name)
		if a[i].Err != b[i].Err {
			fmt.Fprintf(&sb, "  %-8s error: %s\n  %-8s error: %s\n", nameA, a[i].Err, nameB, b[i].Err)
		}
		if !reflect.DeepEqual(a[i].Value, b[i].Value) {
			fmt.Fprintf(&sb, "  %-8s result: %s\n  %-8s result: %s\n", nameA, pretty(a[i].Value), nameB, pretty(b[i].Value))
		}
		t.Error(sb.String())
	}
	if diffs > 0 {
		t.Logf("%d of %d steps differ between the %s and %s stores", diffs, len(script), nameA, nameB)
	}
}

// storeMethods are the methods of the Store interface.
func storeMethods() []string {
	typ := reflect.TypeOf((*Store)(nil)).Elem()
	names := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		names = append(names, typ.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

// checkEveryMethodHasAStep fails when a method of Store is not in the script, or a
// step names one that is not.
func checkEveryMethodHasAStep(t *testing.T, script []step) {
	t.Helper()
	have := map[string]bool{}
	for _, st := range script {
		if st.method != "" {
			have[st.method] = true
		}
	}
	known := map[string]bool{}
	for _, m := range storeMethods() {
		known[m] = true
		if !have[m] {
			t.Errorf("Store.%s has no step in the parity script: add one, so the two stores are held to the same behaviour", m)
		}
	}
	for m := range have {
		if !known[m] {
			t.Errorf("a parity step names Store.%s, which is not a method of Store", m)
		}
	}
}

var bg = context.Background()

// TestParityScriptCoversStore needs no database.
func TestParityScriptCoversStore(t *testing.T) {
	checkEveryMethodHasAStep(t, paritySteps())
}

// The harness itself must be deterministic, or a difference between the stores
// could be noise: the script played on two fresh memory stores must agree.
func TestParityScriptIsDeterministic(t *testing.T) {
	script := paritySteps()
	a := play(t, NewMemoryStore(), script)
	b := play(t, NewMemoryStore(), script)
	compare(t, script, "memory", a, "memory#2", b)
}

// TestPostgresParityWithMemory is the parity test; it runs where DATABASE_URL
// names a server (CI's Postgres job).
func TestPostgresParityWithMemory(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	script := paritySteps()
	mem := play(t, NewMemoryStore(), script)
	pg := play(t, newPostgresTestStore(t), script)
	compare(t, script, "memory", mem, "postgres", pg)
}
