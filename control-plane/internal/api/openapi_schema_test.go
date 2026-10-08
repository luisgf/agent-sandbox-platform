package api

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/openapi"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// schemaTypes names, for each schema of openapi.yaml, the Go type whose JSON it describes. Every schema
// is here or in schemasWithoutAGoType: adding one to the document makes a test ask which.
var schemaTypes = map[string]reflect.Type{
	"Error":                     reflect.TypeOf(errorResponse{}),
	"PlacementError":            reflect.TypeOf(placementErrorResponse{}),
	"OIDCTokenRequest":          reflect.TypeOf(oidcTokenRequest{}),
	"OIDCToken":                 reflect.TypeOf(oidcTokenResponse{}),
	"Sandbox":                   reflect.TypeOf(store.Sandbox{}),
	"CreateSandboxRequest":      reflect.TypeOf(store.CreateSandboxInput{}),
	"SandboxList":               reflect.TypeOf(listSandboxesResponse{}),
	"SandboxEvent":              reflect.TypeOf(store.SandboxEvent{}),
	"SandboxEventList":          reflect.TypeOf(listEventsResponse{}),
	"ExecRequest":               reflect.TypeOf(execRequest{}),
	"ExecResponse":              reflect.TypeOf(execResponse{}),
	"ExecStdinRequest":          reflect.TypeOf(execStdinRequest{}),
	"ExecStdinAck":              reflect.TypeOf(execStdinAck{}),
	"LocalNetGrant":             reflect.TypeOf(localNetGrantResponse{}),
	"LocalNetHeartbeatRequest":  reflect.TypeOf(localNetHeartbeatRequest{}),
	"LocalNetNodePublicRequest": reflect.TypeOf(localNetNodePublicRequest{}),
	"EgressRule":                reflect.TypeOf(store.EgressRule{}),
	"EgressRuleInput":           reflect.TypeOf(egressRuleRequest{}),
	"PutEgressRequest":          reflect.TypeOf(putEgressRequest{}),
	"EgressPolicy":              reflect.TypeOf(store.EgressPolicy{}),
	"EgressRules":               reflect.TypeOf(egressRulesResponse{}),
	"EgressCheckRequest":        reflect.TypeOf(egressCheckRequest{}),
	"EgressCheck":               reflect.TypeOf(egressCheckResponse{}),
	"BootStatement":             reflect.TypeOf(attest.BootStatement{}),
	"Evidence":                  reflect.TypeOf(attest.Evidence{}),
	"AttestRequest":             reflect.TypeOf(attestRequest{}),
	"AttestationRecord":         reflect.TypeOf(store.AttestationRecord{}),
	"AttestationResult":         reflect.TypeOf(attestationResult{}),
	"VerifyAttestationRequest":  reflect.TypeOf(verifyAttestRequest{}),
	"VerifyAttestationResult":   reflect.TypeOf(verifyAttestationResult{}),
	"ClaimRequest":              reflect.TypeOf(claimRequest{}),
	"StatusRequest":             reflect.TypeOf(statusRequest{}),
	"NodeUsage":                 reflect.TypeOf(store.NodeUsage{}),
	"Node":                      reflect.TypeOf(store.Node{}),
	"NodeView":                  reflect.TypeOf(nodeView{}),
	"NodeList":                  reflect.TypeOf(listNodesResponse{}),
	"RegisterNodeRequest":       reflect.TypeOf(store.RegisterNodeInput{}),
	"EnrollNodeRequest":         reflect.TypeOf(store.EnrollNodeInput{}),
	"EnrollResponse":            reflect.TypeOf(enrollResponse{}),
	"EnrollTokenRequest":        reflect.TypeOf(enrollTokenRequest{}),
	"EnrollToken":               reflect.TypeOf(enrollTokenResponse{}),
	"HeartbeatRequest":          reflect.TypeOf(heartbeatRequest{}),
	"NodeWork":                  reflect.TypeOf(listWorkResponse{}),
	"WorkEgress":                reflect.TypeOf(workEgress{}),
	"WorkEgressPolicy":          reflect.TypeOf(workEgressPolicy{}),
	"SetFenceRequest":           reflect.TypeOf(setFenceRequest{}),
	"CreateApiKeyRequest":       reflect.TypeOf(createAPIKeyRequest{}),
	"ApiKey":                    reflect.TypeOf(store.ApiKey{}),
	"ApiKeyWithSecret":          reflect.TypeOf(apiKeyResponse{}),
	"ApiKeyList":                reflect.TypeOf(listAPIKeysResponse{}),
}

// schemasWithoutAGoType are the schemas the control plane does not model with a type: it writes them
// as a literal, relays them from the node agent, or they are a plain string with a list of values.
var schemasWithoutAGoType = map[string]string{
	"Health":              "written as a literal in Routes",
	"OpenIDConfiguration": "built as a map by the OIDC signer",
	"JWKS":                "built as a map by the OIDC signer",
	"JWK":                 "built as a map by the OIDC signer",
	"ExecStreamEvent":     "written by the guest daemon and relayed",
	"DoctorReport":        "written by the node agent and relayed (node-agent/internal/doctor)",
	"DoctorResult":        "written by the node agent and relayed (node-agent/internal/doctor)",
	"SandboxState":        "a string; its values are checked against store/models.go",
}

func TestEverySchemaHasAGoTypeOrSaysWhyNot(t *testing.T) {
	doc := spec(t)
	for _, name := range doc.Components.Schemas.Keys {
		_, typed := schemaTypes[name]
		_, exempt := schemasWithoutAGoType[name]
		switch {
		case typed && exempt:
			t.Errorf("schema %s is both typed and exempt", name)
		case !typed && !exempt:
			t.Errorf("schema %s has no Go type in schemaTypes and no reason in schemasWithoutAGoType (openapi_schema_test.go)", name)
		}
	}
	for name := range schemaTypes {
		if _, ok := doc.Schema(name); !ok {
			t.Errorf("schemaTypes names %s, which openapi.yaml does not have", name)
		}
	}
	for name := range schemasWithoutAGoType {
		if _, ok := doc.Schema(name); !ok {
			t.Errorf("schemasWithoutAGoType names %s, which openapi.yaml does not have", name)
		}
	}
}

// requestSchemas are the schemas a request body is made of, directly or nested: for them "required" says what
// the control plane demands, not what it always sends, so a field without omitempty is not necessarily required.
func requestSchemas(t *testing.T, doc *openapi.Document) map[string]bool {
	t.Helper()
	reach := map[string]bool{}
	var walk func(s *openapi.Schema)
	walk = func(s *openapi.Schema) {
		if s == nil {
			return
		}
		if s.Ref != "" {
			_, name, ok := openapi.RefName(s.Ref)
			if !ok || reach[name] {
				return
			}
			reach[name] = true
			target, _ := doc.Schema(name)
			walk(target)
			return
		}
		walk(s.Items)
		for _, p := range s.Properties.Values {
			walk(p)
		}
		if s.AdditionalProperties != nil {
			walk(s.AdditionalProperties.Schema)
		}
		for _, part := range s.AllOf {
			walk(part)
		}
	}
	for _, op := range doc.Operations() {
		if op.RequestBody == nil {
			continue
		}
		for _, mt := range op.RequestBody.Content.Values {
			walk(mt.Schema)
		}
	}
	return reach
}

type goField struct {
	name      string
	typ       reflect.Type
	omitEmpty bool
}

// jsonFields lists the fields encoding/json writes for a struct: the tagged ones under their tag name,
// and the fields of an embedded struct as if they were its own.
func jsonFields(t reflect.Type) []goField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []goField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" && opts == "" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, jsonFields(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, goField{name: name, typ: f.Type, omitEmpty: strings.Contains(","+opts+",", ",omitempty,")})
	}
	return out
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

// jsonKind is the JSON type encoding/json writes for a Go type, or "any" when it can be anything.
func jsonKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return "string"
	case t == rawType:
		return "any"
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

func indirect(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// specKind is the JSON type a schema says, following references and allOf.
func specKind(doc *openapi.Document, s *openapi.Schema) string {
	r, _, err := doc.Deref(s)
	if err != nil || r == nil {
		return ""
	}
	if r.Type != "" {
		return r.Type
	}
	if len(r.AllOf) > 0 || r.Properties.Len() > 0 {
		return "object"
	}
	return ""
}

type schemaChecker struct {
	t       *testing.T
	doc     *openapi.Document
	request map[string]bool
}

func (c *schemaChecker) errorf(path, format string, args ...any) {
	c.t.Helper()
	c.t.Errorf("%s: %s", path, fmt.Sprintf(format, args...))
}

// object compares the fields of a Go struct with the properties of the schema that describes it.
func (c *schemaChecker) object(path string, schemaName string, s *openapi.Schema, t reflect.Type) {
	c.t.Helper()
	flat, err := c.doc.Flatten(s)
	if err != nil {
		c.errorf(path, "%v", err)
		return
	}
	fields := map[string]goField{}
	for _, f := range jsonFields(t) {
		fields[f.name] = f
		if _, ok := flat.Properties.Get(f.name); !ok {
			c.errorf(path, "Go field %s (%s) is not a property of the schema", f.name, t)
		}
	}
	required := map[string]bool{}
	for _, r := range flat.Required {
		required[r] = true
	}
	isRequest := c.request[schemaName]
	for _, name := range flat.Properties.Keys {
		f, ok := fields[name]
		if !ok {
			c.errorf(path, "property %s is not a JSON field of %s", name, t)
			continue
		}
		if !isRequest && !f.omitEmpty && !required[name] {
			c.errorf(path+"."+name, "the control plane always writes it (no omitempty) and the schema does not list it in required")
		}
		c.value(path+"."+name, flat.Properties.Values[name], f)
	}
}

// value compares one field with the schema of its property.
func (c *schemaChecker) value(path string, s *openapi.Schema, f goField) {
	c.t.Helper()
	c.typed(path, s, f.typ, f.omitEmpty)
}

func (c *schemaChecker) typed(path string, s *openapi.Schema, t reflect.Type, omitEmpty bool) {
	c.t.Helper()
	resolved, refName, err := c.doc.Deref(s)
	if err != nil {
		c.errorf(path, "%v", err)
		return
	}
	if t.Kind() == reflect.Pointer && !omitEmpty && refName == "" && !resolved.Nullable {
		c.errorf(path, "a pointer written without omitempty can be null, and the schema is not nullable")
	}
	base := indirect(t)
	gk := jsonKind(base)
	sk := specKind(c.doc, s)
	if gk != "any" {
		if sk == "" {
			c.errorf(path, "the schema has no type and the Go type %s is a JSON %s", base, gk)
			return
		}
		if sk != gk {
			c.errorf(path, "the schema says %s and the Go type %s is a JSON %s", sk, base, gk)
			return
		}
	}
	// Formats a client generator takes its types from.
	switch {
	case base == timeType:
		if resolved.Format != "date-time" {
			c.errorf(path, "a time.Time has format date-time in the schema, not %q", resolved.Format)
		}
	case base.Kind() == reflect.Int64 || base.Kind() == reflect.Uint64:
		if resolved.Format != "int64" {
			c.errorf(path, "a %s has format int64 in the schema, not %q", base, resolved.Format)
		}
	case gk == "integer":
		if resolved.Format == "int64" {
			c.errorf(path, "the schema says int64 and the Go type is %s", base)
		}
	}
	switch {
	case refName != "":
		if want, ok := schemaTypes[refName]; ok && gk != "any" && indirect(want) != base && base != timeType && gk == "object" {
			c.errorf(path, "refers to schema %s (%s), the Go type is %s", refName, want, base)
		}
	case gk == "array":
		if resolved.Items == nil {
			c.errorf(path, "an array without items")
			return
		}
		c.typed(path+"[]", resolved.Items, base.Elem(), true)
	case gk == "object" && base.Kind() == reflect.Map:
		if ap := resolved.AdditionalProperties; ap != nil && ap.Schema != nil && indirect(base.Elem()).Kind() != reflect.Interface {
			c.typed(path+"{}", ap.Schema, base.Elem(), true)
		}
	case gk == "object" && base.Kind() == reflect.Struct && base != timeType:
		c.object(path, "", resolved, base)
	}
}

func TestSchemasDescribeTheGoTypesTheyAreServedFrom(t *testing.T) {
	doc := spec(t)
	c := &schemaChecker{t: t, doc: doc, request: requestSchemas(t, doc)}
	for _, name := range sortedKeys(schemaTypes) {
		s, ok := doc.Schema(name)
		if !ok {
			continue
		}
		goType := schemaTypes[name]
		if k := jsonKind(goType); k != "object" {
			t.Errorf("schema %s: the Go type %s is a JSON %s, not an object", name, goType, k)
			continue
		}
		c.object(name, name, s, goType)
	}
}

// constStrings returns the string values of the constants of a Go file whose name starts with prefix.
func constStrings(t *testing.T, file, prefix string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if !strings.HasPrefix(name.Name, prefix) || i >= len(vs.Values) {
				continue
			}
			if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				v, _ := strconv.Unquote(lit.Value)
				out = append(out, v)
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("%s: found no constant %s*", file, prefix)
	}
	return out
}

func enumOf(s *openapi.Schema) []string {
	var out []string
	for _, v := range s.Enum {
		out = append(out, fmt.Sprint(v))
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

// TestEnumsAreTheValuesTheCodeUses compares the lists of values the document gives with the constants they
// are written from, so a new state or a new reason cannot be left out of the contract.
func TestEnumsAreTheValuesTheCodeUses(t *testing.T) {
	doc := spec(t)
	prop := func(schema, field string) *openapi.Schema {
		t.Helper()
		s, ok := doc.Schema(schema)
		if !ok {
			t.Fatalf("no schema %s", schema)
		}
		flat, err := doc.Flatten(s)
		if err != nil {
			t.Fatal(err)
		}
		p, ok := flat.Properties.Get(field)
		if !ok {
			t.Fatalf("no property %s.%s", schema, field)
		}
		r, _, err := doc.Deref(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	whole := func(schema string) *openapi.Schema {
		s, _ := doc.Schema(schema)
		return s
	}
	for _, c := range []struct {
		what   string
		schema *openapi.Schema
		file   string
		prefix string
	}{
		{"sandbox states", whole("SandboxState"), "../store/models.go", "Sandbox"},
		{"scheduler reasons (unschedulable_reason)", prop("NodeView", "unschedulable_reason"), "../sched/sched.go", "Reason"},
		{"egress modes", prop("EgressPolicy", "mode"), "../store/egress.go", "EgressMode"},
		{"API key scopes", prop("ApiKey", "scope"), "../store/models.go", "APIKeyScope"},
		{"local-net states", prop("Sandbox", "local_net_state"), "../store/localnet.go", "LocalNet"},
	} {
		if got, want := enumOf(c.schema), constStrings(t, c.file, c.prefix); !sameSet(got, want) {
			t.Errorf("%s: openapi.yaml lists %v, the code has %v", c.what, got, want)
		}
	}
	// The reasons a refused placement counts are the same ones.
	if desc := prop("PlacementError", "reasons").Description; desc != "" {
		for _, r := range constStrings(t, "../sched/sched.go", "Reason") {
			if !strings.Contains(desc, "`"+r+"`") {
				t.Errorf("PlacementError.reasons does not mention the reason %s", r)
			}
		}
	}
}
