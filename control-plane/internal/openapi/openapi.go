// Package openapi reads the OpenAPI 3.0 document of the control-plane API (internal/api/openapi.yaml):
// enough of the format to serve it as JSON, to check it against the code and to write its reference
// page. It models what that document uses and refuses the rest, so a key that is misspelled or not
// supported is an error, and not a field that quietly does nothing.
package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Ordered is a YAML mapping that keeps the order its keys were written in, which is the order a reader
// expects the reference page to follow.
type Ordered[V any] struct {
	Keys   []string
	Values map[string]V
}

// Get returns the value of a key.
func (o Ordered[V]) Get(key string) (V, bool) {
	v, ok := o.Values[key]
	return v, ok
}

// Len is how many keys there are.
func (o Ordered[V]) Len() int { return len(o.Keys) }

// UnmarshalYAML reads a mapping in order, refusing a key that is repeated or a value that has a field
// the model does not know.
func (o *Ordered[V]) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: want a mapping", n.Line)
	}
	o.Keys = nil
	o.Values = make(map[string]V, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if _, dup := o.Values[key]; dup {
			return fmt.Errorf("line %d: %q is repeated", n.Content[i].Line, key)
		}
		var v V
		if err := decodeStrict(n.Content[i+1], &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		o.Keys = append(o.Keys, key)
		o.Values[key] = v
	}
	return nil
}

// decodeStrict decodes a node into v and fails on a field v does not have. Node.Decode is lenient, so
// the node is written out and read again by a decoder that is not.
func decodeStrict(n *yaml.Node, v any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// Document is an OpenAPI document.
type Document struct {
	OpenAPI    string                `yaml:"openapi"`
	Info       Info                  `yaml:"info"`
	Servers    []Server              `yaml:"servers"`
	Security   []SecurityRequirement `yaml:"security"`
	Tags       []Tag                 `yaml:"tags"`
	Paths      Ordered[PathItem]     `yaml:"paths"`
	Components Components            `yaml:"components"`
}

// Info is the title and the long description of the API.
type Info struct {
	Title       string  `yaml:"title"`
	Version     string  `yaml:"version"`
	Description string  `yaml:"description"`
	License     License `yaml:"license"`
}

// License names the license of the API.
type License struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// Server is a place the API is served from.
type Server struct {
	URL         string `yaml:"url"`
	Description string `yaml:"description"`
}

// Tag groups operations.
type Tag struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// SecurityRequirement is one alternative way to authenticate: every scheme it names is needed.
type SecurityRequirement map[string][]string

// PathItem holds the operations of one path.
type PathItem struct {
	Get    *Operation `yaml:"get"`
	Post   *Operation `yaml:"post"`
	Put    *Operation `yaml:"put"`
	Patch  *Operation `yaml:"patch"`
	Delete *Operation `yaml:"delete"`
}

// Operation is one method of one path.
type Operation struct {
	OperationID string   `yaml:"operationId"`
	Summary     string   `yaml:"summary"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Deprecated  bool     `yaml:"deprecated"`
	// Access is who may call it: public, tenant, operator, admin or node (x-asp-access).
	Access string `yaml:"x-asp-access"`
	// Security replaces the document's when it is set, even to nothing (public).
	Security    *[]SecurityRequirement `yaml:"security"`
	Parameters  []Parameter            `yaml:"parameters"`
	RequestBody *RequestBody           `yaml:"requestBody"`
	Responses   Ordered[Response]      `yaml:"responses"`
}

// Parameter is a path, query or header parameter, or a reference to one.
type Parameter struct {
	Ref         string  `yaml:"$ref"`
	Name        string  `yaml:"name"`
	In          string  `yaml:"in"`
	Required    bool    `yaml:"required"`
	Description string  `yaml:"description"`
	Schema      *Schema `yaml:"schema"`
}

// RequestBody is what a request carries.
type RequestBody struct {
	Description string             `yaml:"description"`
	Required    bool               `yaml:"required"`
	Content     Ordered[MediaType] `yaml:"content"`
}

// MediaType is the schema of a body of one content type.
type MediaType struct {
	Schema *Schema `yaml:"schema"`
}

// Response is one answer of an operation, or a reference to a shared one.
type Response struct {
	Ref         string             `yaml:"$ref"`
	Description string             `yaml:"description"`
	Headers     Ordered[Header]    `yaml:"headers"`
	Content     Ordered[MediaType] `yaml:"content"`
}

// Header is a response header.
type Header struct {
	Description string  `yaml:"description"`
	Schema      *Schema `yaml:"schema"`
}

// Components are the parts the rest of the document refers to.
type Components struct {
	SecuritySchemes Ordered[SecurityScheme] `yaml:"securitySchemes"`
	Parameters      Ordered[Parameter]      `yaml:"parameters"`
	Responses       Ordered[Response]       `yaml:"responses"`
	Schemas         Ordered[*Schema]        `yaml:"schemas"`
}

// SecurityScheme is a way to authenticate.
type SecurityScheme struct {
	Type         string `yaml:"type"`
	Scheme       string `yaml:"scheme"`
	BearerFormat string `yaml:"bearerFormat"`
	Description  string `yaml:"description"`
}

// Schema is a JSON Schema as OpenAPI 3.0 uses it.
type Schema struct {
	Ref                  string           `yaml:"$ref"`
	Type                 string           `yaml:"type"`
	Format               string           `yaml:"format"`
	Description          string           `yaml:"description"`
	Nullable             bool             `yaml:"nullable"`
	Enum                 []any            `yaml:"enum"`
	Required             []string         `yaml:"required"`
	Properties           Ordered[*Schema] `yaml:"properties"`
	Items                *Schema          `yaml:"items"`
	AdditionalProperties *Additional      `yaml:"additionalProperties"`
	AllOf                []*Schema        `yaml:"allOf"`
	Minimum              *float64         `yaml:"minimum"`
	Maximum              *float64         `yaml:"maximum"`
	MinItems             *int             `yaml:"minItems"`
	MaxLength            *int             `yaml:"maxLength"`
	Pattern              string           `yaml:"pattern"`
}

// Additional is the additionalProperties of a schema: true (anything) or a schema (the values of a map).
type Additional struct {
	Any    bool
	Schema *Schema
}

// UnmarshalYAML reads true or a schema.
func (a *Additional) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("line %d: additionalProperties is true, false or a schema", n.Line)
		}
		a.Any = b
		return nil
	}
	a.Schema = &Schema{}
	return decodeStrict(n, a.Schema)
}

// Parse reads a document. It checks the shape of the document, which keys it may have, and nothing
// about what they say: Check does that.
func Parse(data []byte) (*Document, error) {
	var d Document
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	return &d, nil
}

// JSON converts the YAML of a document to JSON. edit, when set, may change the generic form before it
// is written (the control plane puts its own version there).
func JSON(data []byte, edit func(map[string]any)) ([]byte, error) {
	var v map[string]any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	if edit != nil {
		edit(v)
	}
	return json.Marshal(v)
}

// Op is an operation with the method and the path it answers.
type Op struct {
	Method string // GET, POST…
	Path   string
	*Operation
}

// Pattern is how the Go ServeMux names the route: "POST /v1/sandboxes/{id}".
func (o Op) Pattern() string { return o.Method + " " + o.Path }

// Operations lists every operation in the order the document gives them: by path, and within a path
// post, get, put, patch, delete (create before list).
func (d *Document) Operations() []Op {
	var ops []Op
	for _, path := range d.Paths.Keys {
		item := d.Paths.Values[path]
		for _, m := range []struct {
			method string
			op     *Operation
		}{{"POST", item.Post}, {"GET", item.Get}, {"PUT", item.Put}, {"PATCH", item.Patch}, {"DELETE", item.Delete}} {
			if m.op != nil {
				ops = append(ops, Op{Method: m.method, Path: path, Operation: m.op})
			}
		}
	}
	return ops
}

// RefName is the name a local reference points at ("#/components/schemas/Sandbox" is "Sandbox"), and
// the kind of component it is in.
func RefName(ref string) (kind, name string, ok bool) {
	rest, found := strings.CutPrefix(ref, "#/components/")
	if !found {
		return "", "", false
	}
	kind, name, found = strings.Cut(rest, "/")
	return kind, name, found && name != ""
}

// Schema finds a component schema by name.
func (d *Document) Schema(name string) (*Schema, bool) { return d.Components.Schemas.Get(name) }

// Deref follows the reference of a schema, if it is one, to the component schema it names.
func (d *Document) Deref(s *Schema) (*Schema, string, error) {
	name := ""
	for hops := 0; s != nil && s.Ref != ""; hops++ {
		kind, n, ok := RefName(s.Ref)
		if !ok || kind != "schemas" {
			return nil, "", fmt.Errorf("%q is not a reference to a schema", s.Ref)
		}
		target, found := d.Schema(n)
		if !found {
			return nil, "", fmt.Errorf("%q: no such schema", s.Ref)
		}
		if hops > 16 {
			return nil, "", fmt.Errorf("%q: references loop", s.Ref)
		}
		name, s = n, target
	}
	return s, name, nil
}

// Flat is the properties and required fields of a schema with its references and allOf resolved.
type Flat struct {
	Properties Ordered[*Schema]
	Required   []string
}

// Flatten merges the properties of a schema, of the schemas it references and of the parts of its allOf.
func (d *Document) Flatten(s *Schema) (Flat, error) {
	flat := Flat{Properties: Ordered[*Schema]{Values: map[string]*Schema{}}}
	return flat, d.flatten(s, &flat, 0)
}

func (d *Document) flatten(s *Schema, into *Flat, depth int) error {
	if depth > 16 {
		return fmt.Errorf("schemas nest too deep")
	}
	s, _, err := d.Deref(s)
	if err != nil || s == nil {
		return err
	}
	for _, part := range s.AllOf {
		if err := d.flatten(part, into, depth+1); err != nil {
			return err
		}
	}
	for _, key := range s.Properties.Keys {
		if _, seen := into.Properties.Values[key]; !seen {
			into.Properties.Keys = append(into.Properties.Keys, key)
		}
		into.Properties.Values[key] = s.Properties.Values[key]
	}
	for _, r := range s.Required {
		if !contains(into.Required, r) {
			into.Required = append(into.Required, r)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Parameters returns the parameters of an operation with the references resolved.
func (d *Document) Parameters(op Op) ([]Parameter, error) {
	out := make([]Parameter, 0, len(op.Parameters))
	for _, p := range op.Parameters {
		if p.Ref != "" {
			kind, name, ok := RefName(p.Ref)
			if !ok || kind != "parameters" {
				return nil, fmt.Errorf("%s: %q is not a reference to a parameter", op.Pattern(), p.Ref)
			}
			target, found := d.Components.Parameters.Get(name)
			if !found {
				return nil, fmt.Errorf("%s: %q: no such parameter", op.Pattern(), p.Ref)
			}
			p = target
		}
		out = append(out, p)
	}
	return out, nil
}

// Response returns an answer with its reference resolved, and the name of the shared response it was.
func (d *Document) Response(r Response) (Response, string, error) {
	if r.Ref == "" {
		return r, "", nil
	}
	kind, name, ok := RefName(r.Ref)
	if !ok || kind != "responses" {
		return r, "", fmt.Errorf("%q is not a reference to a response", r.Ref)
	}
	target, found := d.Components.Responses.Get(name)
	if !found {
		return r, "", fmt.Errorf("%q: no such response", r.Ref)
	}
	return target, name, nil
}

// EffectiveSecurity is how an operation authenticates: its own list when it has one (empty means no
// credential), else the document's.
func (d *Document) EffectiveSecurity(op Op) []SecurityRequirement {
	if op.Security != nil {
		return *op.Security
	}
	return d.Security
}
