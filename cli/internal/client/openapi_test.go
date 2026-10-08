package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// The control plane describes its API in control-plane/internal/api/openapi.yaml, and its own tests keep that
// description true. These tests hold this client to it: the types it decodes and encodes name only fields the
// API has, and every call it makes is a route of the API with a body, a query and an expected status the
// document allows. A client that drifts from the API fails here, not against a real control plane.

const specFile = "../../../control-plane/internal/api/openapi.yaml"

type spec struct{ root map[string]any }

func loadSpec(t *testing.T) *spec {
	t.Helper()
	raw, err := os.ReadFile(specFile)
	if err != nil {
		t.Skipf("no control plane next to this module (it is built outside the repository): %v", err)
	}
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("%s: %v", specFile, err)
	}
	return &spec{root: root}
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// at follows keys down the document.
func (s *spec) at(keys ...string) map[string]any {
	cur := s.root
	for _, k := range keys {
		cur = obj(cur[k])
		if cur == nil {
			return nil
		}
	}
	return cur
}

// deref follows a local reference to the component it names.
func (s *spec) deref(n map[string]any) map[string]any {
	for hops := 0; n != nil && n["$ref"] != nil && hops < 16; hops++ {
		ref, _ := n["$ref"].(string)
		parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
		n = s.at(parts...)
	}
	return n
}

// properties and required merge a schema's own with those of the schemas it refers to and its allOf.
func (s *spec) properties(n map[string]any) (props map[string]map[string]any, required map[string]bool) {
	props, required = map[string]map[string]any{}, map[string]bool{}
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		n = s.deref(n)
		if n == nil {
			return
		}
		for _, part := range list(n["allOf"]) {
			walk(obj(part))
		}
		for name, p := range obj(n["properties"]) {
			props[name] = obj(p)
		}
		for _, r := range list(n["required"]) {
			required[fmt.Sprint(r)] = true
		}
	}
	walk(n)
	return props, required
}

// kind is the JSON type of a schema, "" if it says none.
func (s *spec) kind(n map[string]any) string {
	n = s.deref(n)
	if n == nil {
		return ""
	}
	if t, ok := n["type"].(string); ok {
		return t
	}
	if n["allOf"] != nil || n["properties"] != nil {
		return "object"
	}
	return ""
}

// goKind is the JSON type a Go type is written as and read from, "any" when it can be anything.
func goKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(time.Time{}) {
		return "string"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	}
	return "any"
}

// wireTypes are the types of this package that go over the wire, with the schema each one is read from or
// written as. The client uses a subset of an API's fields, so a type may omit properties; what it may not do is
// name one the API does not have, or give it another type. extra lists the names that are on purpose.
var wireTypes = []struct {
	typ    reflect.Type
	schema string
	extra  map[string]string
}{
	{reflect.TypeOf(Sandbox{}), "Sandbox", nil},
	{reflect.TypeOf(Node{}), "NodeView", nil},
	{reflect.TypeOf(NodeUsage{}), "NodeUsage", nil},
	{reflect.TypeOf(CreateInput{}), "CreateSandboxRequest", nil},
	{reflect.TypeOf(ExecRequest{}), "ExecRequest", nil},
	{reflect.TypeOf(ExecResult{}), "ExecResponse", nil},
	{reflect.TypeOf(listSandboxesResponse{}), "SandboxList", nil},
	{reflect.TypeOf(DoctorResult{}), "DoctorResult", nil},
	{reflect.TypeOf(DoctorReport{}), "DoctorReport", nil},
	{reflect.TypeOf(APIKey{}), "ApiKeyWithSecret", nil},
	{reflect.TypeOf(CreateAPIKeyInput{}), "CreateApiKeyRequest", nil},
	{reflect.TypeOf(EnrollToken{}), "EnrollToken", nil},
	{reflect.TypeOf(LocalNetGrant{}), "LocalNetGrant", nil},
	{reflect.TypeOf(streamEvent{}), "ExecStreamEvent", map[string]string{"stream": "what an older control plane called type"}},
}

func TestClientTypesNameOnlyFieldsTheAPIHas(t *testing.T) {
	s := loadSpec(t)
	for _, w := range wireTypes {
		schema := s.at("components", "schemas", w.schema)
		if schema == nil {
			t.Errorf("%s: openapi.yaml has no schema %s", w.typ, w.schema)
			continue
		}
		props, _ := s.properties(schema)
		for i := 0; i < w.typ.NumField(); i++ {
			f := w.typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			p, ok := props[name]
			if !ok {
				if why, on := w.extra[name]; on {
					_ = why
					continue
				}
				t.Errorf("%s.%s: schema %s has no property %q", w.typ, f.Name, w.schema, name)
				continue
			}
			if gk, sk := goKind(f.Type), s.kind(p); gk != "any" && sk != gk {
				t.Errorf("%s.%s: schema %s says %q is %q, the Go type %s is a JSON %s", w.typ, f.Name, w.schema, name, sk, f.Type, gk)
			}
		}
	}
}

type specOp struct {
	method, path string
	node         map[string]any
	re           *regexp.Regexp
}

func (s *spec) operations() []specOp {
	var out []specOp
	for path, item := range s.at("paths") {
		re := regexp.MustCompile("^" + regexp.MustCompile(`\\\{[^}]*\\\}`).ReplaceAllString(regexp.QuoteMeta(path), "[^/]+") + "$")
		for method, op := range obj(item) {
			out = append(out, specOp{strings.ToUpper(method), path, obj(op), re})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path+out[i].method < out[j].path+out[j].method })
	return out
}

func (s *spec) find(method, path string) (specOp, bool) {
	for _, op := range s.operations() {
		if op.method == method && op.re.MatchString(path) {
			return op, true
		}
	}
	return specOp{}, false
}

// parameters returns the query parameters of an operation, by name.
func (s *spec) queryParameters(op specOp) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, p := range list(op.node["parameters"]) {
		p := s.deref(obj(p))
		if p["in"] == "query" {
			out[fmt.Sprint(p["name"])] = p
		}
	}
	return out
}

// sample builds a JSON value that fills a schema: every property, with values of the right type.
func (s *spec) sample(n map[string]any, depth int) any {
	n = s.deref(n)
	if n == nil || depth > 8 {
		return nil
	}
	if enum := list(n["enum"]); len(enum) > 0 {
		return enum[0]
	}
	switch s.kind(n) {
	case "object":
		out := map[string]any{}
		props, _ := s.properties(n)
		for name, p := range props {
			out[name] = s.sample(p, depth+1)
		}
		return out
	case "array":
		return []any{s.sample(obj(n["items"]), depth+1)}
	case "string":
		if n["format"] == "date-time" {
			return "2026-01-02T03:04:05Z"
		}
		return "x"
	case "integer":
		return 1
	case "number":
		return 1.5
	case "boolean":
		return true
	}
	return nil
}

// specServer answers like the API the document describes, and writes down every complaint about the calls it gets.
type specServer struct {
	*httptest.Server
	t      *testing.T
	spec   *spec
	mu     sync.Mutex
	called map[string]bool
}

func newSpecServer(t *testing.T, s *spec) *specServer {
	t.Helper()
	ss := &specServer{t: t, spec: s, called: map[string]bool{}}
	ss.Server = httptest.NewServer(http.HandlerFunc(ss.serve))
	t.Cleanup(ss.Close)
	return ss
}

func (ss *specServer) complain(r *http.Request, format string, args ...any) {
	ss.t.Helper()
	ss.t.Errorf("%s %s: %s", r.Method, r.URL.RequestURI(), fmt.Sprintf(format, args...))
}

func (ss *specServer) serve(w http.ResponseWriter, r *http.Request) {
	op, ok := ss.spec.find(r.Method, r.URL.Path)
	if !ok {
		ss.complain(r, "openapi.yaml has no such route")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ss.mu.Lock()
	ss.called[op.method+" "+op.path] = true
	ss.mu.Unlock()

	// The query names only parameters the operation has, with values it accepts.
	params := ss.spec.queryParameters(op)
	for name, values := range r.URL.Query() {
		p, ok := params[name]
		if !ok {
			ss.complain(r, "the operation has no query parameter %q", name)
			continue
		}
		if enum := list(obj(p["schema"])["enum"]); len(enum) > 0 {
			found := false
			for _, e := range enum {
				if fmt.Sprint(e) == values[0] {
					found = true
				}
			}
			if !found {
				ss.complain(r, "query parameter %q = %q, the operation accepts %v", name, values[0], enum)
			}
		}
	}

	// The body names only properties of the request schema, and carries the ones it requires.
	body, _ := io.ReadAll(r.Body)
	if rb := obj(op.node["requestBody"]); rb != nil {
		schema := obj(obj(obj(rb["content"])["application/json"])["schema"])
		props, required := ss.spec.properties(schema)
		if len(body) == 0 {
			if rb["required"] == true {
				ss.complain(r, "the operation needs a body and the client sent none")
			}
		} else {
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(body, &sent); err != nil {
				ss.complain(r, "the body is not a JSON object: %v", err)
			}
			for name := range sent {
				if _, ok := props[name]; !ok {
					ss.complain(r, "the body has %q, which the request schema does not have", name)
				}
			}
			for name := range required {
				if _, ok := sent[name]; !ok {
					ss.complain(r, "the body lacks %q, which the request schema requires", name)
				}
			}
		}
	} else if len(body) > 0 {
		ss.complain(r, "the operation takes no body and the client sent %q", body)
	}

	// Answer with the lowest 2xx status the operation lists, and a body that fills its schema.
	status := 0
	for code := range obj(op.node["responses"]) {
		var n int
		fmt.Sscanf(code, "%d", &n)
		if n >= 200 && n < 300 && (status == 0 || n < status) {
			status = n
		}
	}
	if status == 0 {
		ss.complain(r, "the operation lists no 2xx status")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	resp := ss.spec.deref(obj(obj(op.node["responses"])[fmt.Sprint(status)]))
	content := obj(resp["content"])
	switch {
	case r.URL.Query().Get("stream") != "" && content["application/x-ndjson"] != nil:
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"type":"stdout","data":"hello\n"}`+"\n"+`{"type":"stderr","data":"warn\n"}`+"\n"+`{"type":"exit","exit_code":3}`+"\n")
	case content["application/json"] != nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(ss.spec.sample(obj(obj(content["application/json"])["schema"]), 0))
	default:
		w.WriteHeader(status)
	}
}

// calls is every method of Client that talks to the API, called the way the CLI calls it.
var calls = map[string]func(c *Client) error{
	"CreateSandbox": func(c *Client) error {
		local := true
		_, err := c.CreateSandbox(context.Background(), CreateInput{TenantID: "t", ImageRef: "debian:bookworm-slim", CPUMillis: 1000, MemoryMiB: 512,
			VMMProfile: "cloud-hypervisor", NodeID: "n1", WorkspaceHostPath: "/w/t/p", LocalNet: &local})
		return err
	},
	"GetSandbox": func(c *Client) error { _, err := c.GetSandbox(context.Background(), "sbx"); return err },
	"ListSandboxes": func(c *Client) error {
		_, err := c.ListSandboxes(context.Background(), "t")
		return err
	},
	"ListSandboxesAll": func(c *Client) error {
		_, err := c.ListSandboxesAll(context.Background(), "t", true)
		return err
	},
	"DeleteSandbox": func(c *Client) error { _, err := c.DeleteSandbox(context.Background(), "sbx"); return err },
	"StopSandbox":   func(c *Client) error { _, err := c.StopSandbox(context.Background(), "sbx"); return err },
	"StartSandbox":  func(c *Client) error { _, err := c.StartSandbox(context.Background(), "sbx"); return err },
	"Exec": func(c *Client) error {
		_, err := c.Exec(context.Background(), "sbx", ExecRequest{Cmd: []string{"echo", "hi"}, Env: map[string]string{"A": "b"}, Cwd: "/w", Stdin: "in",
			AsRoot: true, TimeoutSeconds: 30})
		return err
	},
	"ExecStream": func(c *Client) error {
		var out, errOut strings.Builder
		code, err := c.ExecStream(context.Background(), "sbx", ExecRequest{Cmd: []string{"echo", "hi"}, PTY: true, Rows: 24, Cols: 80, StdinStream: true}, &out, &errOut)
		if err == nil && (code != 3 || out.String() != "hello\n" || errOut.String() != "warn\n") {
			return fmt.Errorf("the stream the document describes was read as code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
		}
		return err
	},
	"pumpExecStdin": func(c *Client) error {
		// The stdin of a stream is posted to /exec/stdin: input, then the end of input.
		stop := make(chan struct{})
		return c.pumpExecStdin(context.Background(), "sbx", "exec-1", strings.NewReader("data"), stop)
	},
	"IssueLocalNetGrant": func(c *Client) error { _, err := c.IssueLocalNetGrant(context.Background(), "sbx"); return err },
	"HeartbeatLocalNet": func(c *Client) error {
		_, err := c.HeartbeatLocalNet(context.Background(), "sbx", "grant", "pub")
		return err
	},
	"DetachLocalNet": func(c *Client) error { _, err := c.DetachLocalNet(context.Background(), "sbx"); return err },
	"ListNodes":      func(c *Client) error { _, err := c.ListNodes(context.Background()); return err },
	"NodeDoctor":     func(c *Client) error { _, err := c.NodeDoctor(context.Background(), "n1"); return err },
	"SetNodeCordoned": func(c *Client) error {
		if _, err := c.SetNodeCordoned(context.Background(), "n1", true); err != nil {
			return err
		}
		_, err := c.SetNodeCordoned(context.Background(), "n1", false)
		return err
	},
	"SetNodeFence": func(c *Client) error {
		return c.SetNodeFence(context.Background(), "n1", "https://bmc.example/off", "env:FENCE")
	},
	"ClearNodeFence": func(c *Client) error { return c.ClearNodeFence(context.Background(), "n1") },
	"CreateEnrollToken": func(c *Client) error {
		_, err := c.CreateEnrollToken(context.Background(), "n1", time.Hour)
		return err
	},
	"CreateAPIKey": func(c *Client) error {
		_, err := c.CreateAPIKey(context.Background(), CreateAPIKeyInput{TenantID: "t", Name: "ci", Scope: "tenant", TTL: "30d"})
		return err
	},
	"ListAPIKeys":  func(c *Client) error { _, err := c.ListAPIKeys(context.Background(), "t"); return err },
	"RevokeAPIKey": func(c *Client) error { _, err := c.RevokeAPIKey(context.Background(), "k1"); return err },
	"RotateAPIKey": func(c *Client) error { _, err := c.RotateAPIKey(context.Background(), "k1"); return err },
}

// notCalls are the exported methods of Client that are not in calls, and why.
var notCalls = map[string]string{
	"TrustCAFile":  "makes no request",
	"SetBearer":    "makes no request",
	"ExecStreamIO": "ExecStream is ExecStreamIO without stdin; the posts of its stdin are pumpExecStdin",
}

func TestEveryClientCallIsARouteOfTheAPI(t *testing.T) {
	s := loadSpec(t)
	ss := newSpecServer(t, s)
	c := New(ss.URL, "key")
	for _, name := range sortedNames(calls) {
		if err := calls[name](c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func sortedNames(m map[string]func(*Client) error) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// A method added to Client must be added to calls, or say it makes no request, so that it is held to the document too.
func TestEveryClientMethodIsChecked(t *testing.T) {
	ct := reflect.TypeOf(&Client{})
	for i := 0; i < ct.NumMethod(); i++ {
		name := ct.Method(i).Name
		if _, ok := calls[name]; ok {
			continue
		}
		if _, ok := notCalls[name]; ok {
			continue
		}
		t.Errorf("Client.%s is not in calls (openapi_test.go): add the way the CLI calls it, or list it in notCalls if it makes no request", name)
	}
	for name := range calls {
		if _, ok := ct.MethodByName(name); !ok && name[0] >= 'A' && name[0] <= 'Z' {
			t.Errorf("calls names %s, which Client does not have", name)
		}
	}
}
