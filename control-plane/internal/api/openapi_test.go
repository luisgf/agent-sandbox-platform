package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/openapi"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/version"
)

// openapi.yaml is the contract of the API, and these tests keep it true: its form (openapi.Check), its
// routes against routes.go, the fields of its bodies against the Go types that carry them, the statuses
// it lists against the ones the handlers answer, and the who-may-call-it of each operation against the
// authentication middleware. The reference page is generated from it (cmd/api/reference_test.go).

var loadSpec = sync.OnceValues(func() (*openapi.Document, error) { return openapi.Parse(OpenAPISpec()) })

func spec(t testing.TB) *openapi.Document {
	t.Helper()
	doc, err := loadSpec()
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestOpenAPIDocumentIsWellFormed(t *testing.T) {
	for _, problem := range spec(t).Check() {
		t.Error(problem)
	}
}

func TestOpenAPIIsServedAsJSONWithoutACredential(t *testing.T) {
	// A control plane that refuses every request without a credential still serves its own description.
	srv := NewServer(newTestStore(t))
	h := AuthMiddleware(srv.Store, AuthConfig{})(testMux(srv))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got struct {
		OpenAPI string                    `json:"openapi"`
		Info    struct{ Version string }  `json:"info"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	doc := spec(t)
	if got.OpenAPI != doc.OpenAPI {
		t.Errorf("openapi = %q, want %q", got.OpenAPI, doc.OpenAPI)
	}
	if got.Info.Version != version.Short() {
		t.Errorf("info.version = %q, want the build's %q", got.Info.Version, version.Short())
	}
	if len(got.Paths) != doc.Paths.Len() {
		t.Errorf("%d paths served, the document has %d", len(got.Paths), doc.Paths.Len())
	}
	// The JSON keeps what a client generator needs.
	if _, ok := got.Paths["/v1/sandboxes/{id}/exec"]["post"]; !ok {
		t.Error("POST /v1/sandboxes/{id}/exec is not in the JSON")
	}
}

// registeredRoutes reads routes.go and returns each pattern with the name of its handler.
func registeredRoutes(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "routes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s: a route pattern that is not a string literal", fset.Position(call.Pos()))
			return true
		}
		pattern, _ := strconv.Unquote(lit.Value)
		handler := "(inline)"
		if h, ok := call.Args[1].(*ast.SelectorExpr); ok {
			handler = h.Sel.Name
		}
		routes[pattern] = handler
		return true
	})
	if len(routes) == 0 {
		t.Fatal("found no route in routes.go")
	}
	return routes
}

func TestEveryRouteIsInTheSpecAndTheOtherWayRound(t *testing.T) {
	doc := spec(t)
	routes := registeredRoutes(t)
	inSpec := map[string]bool{}
	for _, op := range doc.Operations() {
		inSpec[op.Pattern()] = true
		if _, ok := routes[op.Pattern()]; !ok {
			t.Errorf("openapi.yaml documents %s, which routes.go does not serve", op.Pattern())
		}
	}
	for pattern, handler := range routes {
		if !inSpec[pattern] {
			t.Errorf("routes.go serves %s (%s), which openapi.yaml does not document", pattern, handler)
		}
	}
}

// TestTheSpecPathsReachTheRoutesTheyName asks the mux itself which route a documented request lands on,
// so a pattern that routes.go lists but the mux resolves elsewhere (a more specific route shadowing it)
// is caught too.
func TestTheSpecPathsReachTheRoutesTheyName(t *testing.T) {
	mux := NewServer(newTestStore(t)).Routes()
	for _, op := range spec(t).Operations() {
		path := strings.NewReplacer("{id}", "x1").Replace(op.Path)
		req := httptest.NewRequest(op.Method, path, nil)
		if _, pattern := mux.Handler(req); pattern != op.Pattern() {
			t.Errorf("%s %s is served by %q, not by %q", op.Method, path, pattern, op.Pattern())
		}
	}
}

func TestAccessMatchesTheAuthenticationMiddleware(t *testing.T) {
	for _, op := range spec(t).Operations() {
		path := strings.NewReplacer("{id}", "x1").Replace(op.Path)
		if got, want := isPublicPath(path), op.Access == "public"; got != want {
			t.Errorf("%s: x-asp-access %q, but isPublicPath(%s) = %v", op.Pattern(), op.Access, path, got)
		}
		if got, want := isNodeAgentPath(path), op.Access == "node"; got != want {
			t.Errorf("%s: x-asp-access %q, but isNodeAgentPath(%s) = %v", op.Pattern(), op.Access, path, got)
		}
		if isNodeAdminPath(path) && op.Access != "admin" {
			t.Errorf("%s: the middleware treats %s as node administration, the spec says %q", op.Pattern(), path, op.Access)
		}
	}
}

// statusNames are the net/http constants the handlers write, to read them from the source.
var statusNames = map[string]int{
	"StatusOK": 200, "StatusCreated": 201, "StatusNoContent": 204,
	"StatusBadRequest": 400, "StatusUnauthorized": 401, "StatusForbidden": 403, "StatusNotFound": 404,
	"StatusConflict": 409, "StatusGone": 410, "StatusRequestEntityTooLarge": 413,
	"StatusInternalServerError": 500, "StatusNotImplemented": 501, "StatusBadGateway": 502,
	"StatusServiceUnavailable": 503, "StatusGatewayTimeout": 504,
}

// handlerStatuses reads the source of the package and returns, for each method of Server, the statuses
// it writes itself: writeError(w, http.StatusX, …), writeJSON(w, http.StatusX, …), forbid(w, …) (403) and
// w.WriteHeader(http.StatusX). What a helper writes on its behalf is not here; the statuses the tests see
// (recordAnswers) cover that.
func handlerStatuses(t *testing.T) map[string]map[int]token.Position {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
	}
	out := map[string]map[int]token.Position{}
	for _, file := range parsed {
		{
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil {
					continue
				}
				found := map[int]token.Position{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					name := ""
					switch f := call.Fun.(type) {
					case *ast.Ident:
						name = f.Name
					case *ast.SelectorExpr:
						name = f.Sel.Name
					}
					statusArg := -1
					switch name {
					case "writeError", "writeJSON":
						statusArg = 1
					case "WriteHeader":
						statusArg = 0
					case "forbid":
						found[403] = fset.Position(call.Pos())
						return true
					}
					if statusArg < 0 || len(call.Args) <= statusArg {
						return true
					}
					if sel, ok := call.Args[statusArg].(*ast.SelectorExpr); ok {
						code, known := statusNames[sel.Sel.Name]
						if !known {
							t.Errorf("%s: %s is not in statusNames (openapi_test.go): add it", fset.Position(call.Pos()), sel.Sel.Name)
							return true
						}
						if _, seen := found[code]; !seen {
							found[code] = fset.Position(call.Pos())
						}
					}
					return true
				})
				if len(found) > 0 {
					out[fn.Name.Name] = found
				}
			}
		}
	}
	return out
}

func TestHandlersAnswerOnlyStatusesTheSpecLists(t *testing.T) {
	doc := spec(t)
	routes := registeredRoutes(t)
	written := handlerStatuses(t)
	for _, op := range doc.Operations() {
		handler := routes[op.Pattern()]
		for code, pos := range written[handler] {
			// 400 is how a handler says "the id is empty", which a route with a {id} never sees, and 500 is
			// its store failing: the spec lists them where they can be told apart.
			if code == 400 || code == 500 {
				continue
			}
			if _, ok := op.Responses.Get(strconv.Itoa(code)); !ok {
				t.Errorf("%s: %s writes %d (%s) and openapi.yaml does not list it", op.Pattern(), handler, code, pos)
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var _ = store.ErrNotFound

// answers records, for every route the tests call through testMux, the statuses it answered, so that
// TestMain can check them against the document once every test has run: a status the suite makes a
// handler answer is a status openapi.yaml has to list. The handlers' own source (handlerStatuses) covers
// what the tests do not reach; this covers what the source does not show, such as the statuses a shared
// helper writes.
var answers = struct {
	sync.Mutex
	seen map[string]map[int]string // route pattern -> status -> a path that got it
}{seen: map[string]map[int]string{}}

// recordAnswers wraps the mux so that testMux notes each answer.
func recordAnswers(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		sw := &statusWriter{ResponseWriter: w}
		mux.ServeHTTP(sw, r)
		if pattern == "" {
			return
		}
		answers.Lock()
		defer answers.Unlock()
		if answers.seen[pattern] == nil {
			answers.seen[pattern] = map[int]string{}
		}
		if _, ok := answers.seen[pattern][sw.status()]; !ok {
			answers.seen[pattern][sw.status()] = r.URL.Path
		}
	})
}

// statusWriter remembers the status of an answer and passes everything else through, flushing included
// (a streamed exec) and the unwrapping a ResponseController does.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

// undocumentedAnswers lists the statuses the tests saw that the document does not list for the route.
// Called by TestMain after the last test.
func undocumentedAnswers() []string {
	doc, err := loadSpec()
	if err != nil {
		return []string{err.Error()}
	}
	listed := map[string]map[string]bool{}
	for _, op := range doc.Operations() {
		listed[op.Pattern()] = map[string]bool{}
		for _, code := range op.Responses.Keys {
			listed[op.Pattern()][code] = true
		}
	}
	answers.Lock()
	defer answers.Unlock()
	var out []string
	for pattern, codes := range answers.seen {
		for code, path := range codes {
			if !listed[pattern][strconv.Itoa(code)] {
				out = append(out, pattern+" answered "+strconv.Itoa(code)+" (on "+path+") and openapi.yaml does not list it")
			}
		}
	}
	sort.Strings(out)
	return out
}
