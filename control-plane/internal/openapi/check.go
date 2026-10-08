package openapi

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Accesses are the values of x-asp-access, who may call an operation.
var Accesses = []string{"public", "tenant", "operator", "admin", "node"}

var (
	pathParam  = regexp.MustCompile(`\{([^}/]+)\}`)
	statusCode = regexp.MustCompile(`^[1-5][0-9][0-9]$`)
)

// Check reads what a document says and reports what does not hold together: a reference to nothing, an
// operation without a summary or a way in, a path parameter that is not declared, a component nothing uses.
// It returns one line per problem, in document order.
func (d *Document) Check() []string {
	c := &checker{doc: d, used: map[string]bool{}}
	c.run()
	return c.problems
}

type checker struct {
	doc      *Document
	problems []string
	// used holds "kind/name" of every component something refers to.
	used map[string]bool
}

func (c *checker) addf(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

func (c *checker) run() {
	d := c.doc
	if !strings.HasPrefix(d.OpenAPI, "3.0.") {
		c.addf("openapi: %q is not an OpenAPI 3.0 document", d.OpenAPI)
	}
	if strings.TrimSpace(d.Info.Title) == "" || strings.TrimSpace(d.Info.Version) == "" || strings.TrimSpace(d.Info.Description) == "" {
		c.addf("info needs a title, a version and a description")
	}
	tags := map[string]bool{}
	for _, t := range d.Tags {
		if tags[t.Name] {
			c.addf("tag %q is declared twice", t.Name)
		}
		tags[t.Name] = true
		if strings.TrimSpace(t.Description) == "" {
			c.addf("tag %q has no description", t.Name)
		}
	}
	c.checkSecurity("the document", d.Security)

	ids := map[string]string{}
	tagUsed := map[string]bool{}
	for _, op := range d.Operations() {
		c.checkOperation(op, tags, tagUsed, ids)
	}
	for _, t := range d.Tags {
		if !tagUsed[t.Name] {
			c.addf("tag %q has no operation", t.Name)
		}
	}

	for _, name := range d.Components.Schemas.Keys {
		c.checkSchema("schema "+name, d.Components.Schemas.Values[name], 0)
	}
	for _, name := range d.Components.Parameters.Keys {
		c.checkParameter("parameter "+name, d.Components.Parameters.Values[name])
	}
	for _, name := range d.Components.Responses.Keys {
		c.checkResponse("response "+name, d.Components.Responses.Values[name])
	}
	for _, name := range d.Components.SecuritySchemes.Keys {
		s := d.Components.SecuritySchemes.Values[name]
		if s.Type != "http" || s.Scheme != "bearer" {
			c.addf("security scheme %s: only http bearer is supported", name)
		}
		if strings.TrimSpace(s.Description) == "" {
			c.addf("security scheme %s has no description", name)
		}
	}
	for kind, comp := range map[string][]string{
		"schemas":    d.Components.Schemas.Keys,
		"parameters": d.Components.Parameters.Keys,
		"responses":  d.Components.Responses.Keys,
	} {
		for _, name := range comp {
			if !c.used[kind+"/"+name] {
				c.addf("%s %s is not used by anything", strings.TrimSuffix(kind, "s"), name)
			}
		}
	}
	sort.Strings(c.problems)
}

func (c *checker) checkSecurity(where string, reqs []SecurityRequirement) {
	for _, r := range reqs {
		for name := range r {
			if _, ok := c.doc.Components.SecuritySchemes.Get(name); !ok {
				c.addf("%s: security scheme %q is not declared", where, name)
			}
		}
	}
}

func (c *checker) checkOperation(op Op, tags, tagUsed map[string]bool, ids map[string]string) {
	where := op.Pattern()
	if op.OperationID == "" {
		c.addf("%s: no operationId", where)
	} else if other, dup := ids[op.OperationID]; dup {
		c.addf("%s: operationId %q is also used by %s", where, op.OperationID, other)
	} else {
		ids[op.OperationID] = where
	}
	if strings.TrimSpace(op.Summary) == "" {
		c.addf("%s: no summary", where)
	}
	if strings.TrimSpace(op.Description) == "" {
		c.addf("%s: no description", where)
	}
	if len(op.Tags) == 0 {
		c.addf("%s: no tag", where)
	}
	for _, t := range op.Tags {
		tagUsed[t] = true
		if !tags[t] {
			c.addf("%s: tag %q is not declared", where, t)
		}
	}
	if !contains(Accesses, op.Access) {
		c.addf("%s: x-asp-access %q is not one of %s", where, op.Access, strings.Join(Accesses, ", "))
	}

	sec := c.doc.EffectiveSecurity(op)
	c.checkSecurity(where, sec)
	switch {
	case op.Access == "public" && op.Security == nil:
		c.addf("%s: a public operation says so with security: [] or its own scheme", where)
	case op.Access != "public" && len(sec) == 0:
		c.addf("%s: x-asp-access %s but no security scheme", where, op.Access)
	}

	// Parameters.
	params, err := c.doc.Parameters(op)
	if err != nil {
		c.addf("%v", err)
	}
	seen := map[string]bool{}
	declared := map[string]bool{}
	for _, p := range op.Parameters {
		if p.Ref != "" {
			if kind, name, ok := RefName(p.Ref); ok {
				c.used[kind+"/"+name] = true
			}
		} else {
			c.checkParameter(where+" parameter "+p.Name, p)
		}
	}
	for _, p := range params {
		key := p.In + " " + p.Name
		if seen[key] {
			c.addf("%s: parameter %s is declared twice", where, key)
		}
		seen[key] = true
		if p.In == "path" {
			declared[p.Name] = true
			if !p.Required {
				c.addf("%s: path parameter %s must be required", where, p.Name)
			}
		}
	}
	inPath := map[string]bool{}
	for _, m := range pathParam.FindAllStringSubmatch(op.Path, -1) {
		inPath[m[1]] = true
		if !declared[m[1]] {
			c.addf("%s: path parameter {%s} is not declared", where, m[1])
		}
	}
	for name := range declared {
		if !inPath[name] {
			c.addf("%s: path parameter %s is not in the path", where, name)
		}
	}

	// Request body.
	if rb := op.RequestBody; rb != nil {
		c.checkContent(where+" request body", rb.Content)
		if rb.Content.Len() == 0 {
			c.addf("%s: request body without content", where)
		}
	}

	// Responses.
	success := false
	has401 := false
	for _, code := range op.Responses.Keys {
		if !statusCode.MatchString(code) {
			c.addf("%s: response %q is not a status code", where, code)
			continue
		}
		if code[0] == '2' {
			success = true
		}
		if code == "401" {
			has401 = true
		}
		r := op.Responses.Values[code]
		c.checkResponse(where+" response "+code, r)
	}
	if !success && !op.Deprecated {
		c.addf("%s: no 2xx response", where)
	}
	if op.Access != "public" && !has401 && !op.Deprecated {
		c.addf("%s: an operation that needs a credential answers 401", where)
	}
}

func (c *checker) checkParameter(where string, p Parameter) {
	switch p.In {
	case "path", "query", "header":
	default:
		c.addf("%s: in %q is not path, query or header", where, p.In)
	}
	if p.Name == "" || strings.TrimSpace(p.Description) == "" {
		c.addf("%s: needs a name and a description", where)
	}
	if p.Schema == nil {
		c.addf("%s: no schema", where)
		return
	}
	c.checkSchema(where, p.Schema, 0)
}

func (c *checker) checkContent(where string, content Ordered[MediaType]) {
	for _, ct := range content.Keys {
		switch ct {
		case "application/json", "application/x-ndjson", "text/plain":
		default:
			c.addf("%s: content type %q is not one this document uses", where, ct)
		}
		m := content.Values[ct]
		if m.Schema == nil {
			c.addf("%s: %s has no schema", where, ct)
			continue
		}
		c.checkSchema(where+" "+ct, m.Schema, 0)
	}
}

func (c *checker) checkResponse(where string, r Response) {
	if r.Ref != "" {
		kind, name, ok := RefName(r.Ref)
		if !ok || kind != "responses" {
			c.addf("%s: %q is not a reference to a response", where, r.Ref)
			return
		}
		if _, found := c.doc.Components.Responses.Get(name); !found {
			c.addf("%s: %q: no such response", where, r.Ref)
			return
		}
		c.used[kind+"/"+name] = true
		return
	}
	if strings.TrimSpace(r.Description) == "" {
		c.addf("%s: no description", where)
	}
	c.checkContent(where, r.Content)
	for _, h := range r.Headers.Keys {
		hd := r.Headers.Values[h]
		if hd.Schema == nil || strings.TrimSpace(hd.Description) == "" {
			c.addf("%s: header %s needs a schema and a description", where, h)
		}
	}
}

func (c *checker) checkSchema(where string, s *Schema, depth int) {
	if s == nil {
		return
	}
	if depth > 24 {
		c.addf("%s: schemas nest too deep", where)
		return
	}
	if s.Ref != "" {
		kind, name, ok := RefName(s.Ref)
		if !ok || kind != "schemas" {
			c.addf("%s: %q is not a reference to a schema", where, s.Ref)
			return
		}
		if _, found := c.doc.Schema(name); !found {
			c.addf("%s: %q: no such schema", where, s.Ref)
			return
		}
		c.used[kind+"/"+name] = true
		return
	}
	for _, part := range s.AllOf {
		c.checkSchema(where, part, depth+1)
	}
	if len(s.AllOf) > 0 {
		return
	}
	switch s.Type {
	case "string", "integer", "number", "boolean":
	case "array":
		if s.Items == nil {
			c.addf("%s: an array without items", where)
		}
	case "object":
	case "":
		// A schema with no type accepts anything; the document uses that for a few fields on purpose.
	default:
		c.addf("%s: type %q", where, s.Type)
	}
	if s.Type != "" && len(s.Enum) == 0 && s.Enum != nil {
		c.addf("%s: an empty enum", where)
	}
	c.checkSchema(where, s.Items, depth+1)
	for _, name := range s.Properties.Keys {
		prop := s.Properties.Values[name]
		c.checkSchema(where+"."+name, prop, depth+1)
		if !c.described(prop) {
			c.addf("%s.%s: a property needs a description (its own, or the schema it refers to)", where, name)
		}
	}
	if s.AdditionalProperties != nil {
		c.checkSchema(where+" values", s.AdditionalProperties.Schema, depth+1)
	}
	flat, err := c.doc.Flatten(s)
	if err != nil {
		c.addf("%s: %v", where, err)
		return
	}
	for _, r := range s.Required {
		if _, ok := flat.Properties.Get(r); !ok {
			c.addf("%s: %q is required and is not a property", where, r)
		}
	}
}

// described says whether the text beside a property in the reference page would not be empty.
func (c *checker) described(p *Schema) bool {
	if p == nil {
		return true
	}
	if strings.TrimSpace(p.Description) != "" {
		return true
	}
	if p.Ref != "" {
		target, _, err := c.doc.Deref(p)
		return err != nil || target == nil || strings.TrimSpace(target.Description) != ""
	}
	return false
}
