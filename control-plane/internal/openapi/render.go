package openapi

import (
	"fmt"
	"strings"
)

// Markdown writes the reference page of the API (docs/reference/api.md) from the document. The text around the
// tables is Spanish, like the rest of the docs; the descriptions are the document's own.
func Markdown(d *Document) (string, error) {
	r := &renderer{doc: d}
	if err := r.page(); err != nil {
		return "", err
	}
	return r.b.String(), nil
}

type renderer struct {
	doc *Document
	b   strings.Builder
	err error
}

func (r *renderer) printf(format string, args ...any) { fmt.Fprintf(&r.b, format, args...) }

func (r *renderer) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// accessText is what each value of x-asp-access means, as the page tells it.
var accessText = map[string]string{
	"public":   "Cualquiera, sin credencial.",
	"tenant":   "Una clave de API o un token del IdP, dentro de su tenant. Con un token del IdP decide el rol: cada operación dice cuál.",
	"operator": "Rol `admin` u `operator` del IdP, o una clave de plataforma.",
	"admin":    "Rol `admin` del IdP, o una clave de plataforma.",
	"node":     "Un node-agent: su certificado de cliente (mTLS) o una clave de plataforma.",
}

var schemeText = map[string]string{
	"apiKey":      "clave de API",
	"idpToken":    "token del IdP",
	"enrollToken": "token de bootstrap o de enroll",
}

func (r *renderer) page() error {
	d := r.doc
	r.printf("# API del plano de control\n\n")
	r.printf("> Esta página se genera de [`openapi.yaml`](../../control-plane/internal/api/openapi.yaml) con `make docs`: no se edita a mano. " +
		"Un test compara el documento con las rutas, los tipos y los estados que devuelve el código, así que no se queda atrás.\n\n")
	r.printf("Referencia de la API HTTP del plano de control, bajo `/v1`. El plano de control sirve el mismo documento como JSON en " +
		"`GET /openapi.json`, sin credenciales, para generar un cliente (`oapi-codegen`, `openapi-generator`) o abrirlo en Swagger UI o Redoc. " +
		"Los textos del documento están en inglés, como los mensajes de error de la API.\n\n")

	r.printf("## Convenciones\n\n")
	r.printf("%s\n\n", demoteHeadings(strings.TrimSpace(d.Info.Description)))

	r.printf("## Quién puede llamar a qué\n\n")
	r.printf("Cada operación lleva un valor de acceso:\n\n| Acceso | Quién puede llamar |\n|---|---|\n")
	for _, a := range Accesses {
		r.printf("| `%s` | %s |\n", a, accessText[a])
	}
	r.printf("\nCon un token del IdP, el rol acota lo que se puede hacer dentro de cada acceso. " +
		"Cómo se conecta un IdP: [conectar un IdP](../how-to/idp.md).\n\n")

	r.printf("## Operaciones\n\n")
	ops := d.Operations()
	for _, tag := range d.Tags {
		r.printf("### %s\n\n%s\n\n", tag.Name, strings.TrimSpace(tag.Description))
		r.printf("| Operación | Qué hace | Acceso |\n|---|---|---|\n")
		var mine []Op
		for _, op := range ops {
			if len(op.Tags) > 0 && op.Tags[0] == tag.Name {
				mine = append(mine, op)
				r.printf("| [`%s`](#%s) | %s | `%s` |\n", op.Pattern(), anchor(op), cell(op.Summary), op.Access)
			}
		}
		r.printf("\n")
		for _, op := range mine {
			r.operation(op)
		}
	}

	r.printf("## Esquemas\n\n")
	r.printf("Los cuerpos JSON que las operaciones reciben y devuelven. «Obligatorio» en una respuesta quiere decir que el plano de control " +
		"siempre lo escribe; en una petición, que lo exige.\n\n")
	for _, name := range d.Components.Schemas.Keys {
		r.schema(name, d.Components.Schemas.Values[name])
	}
	return r.err
}

func anchor(op Op) string { return strings.ToLower(op.OperationID) }

func schemaAnchor(name string) string { return "schema-" + strings.ToLower(name) }

func (r *renderer) operation(op Op) {
	d := r.doc
	r.printf("#### `%s` <a id=\"%s\"></a>\n\n", op.Pattern(), anchor(op))
	line := fmt.Sprintf("**%s**. Acceso: `%s`. Autenticación: %s.", strings.TrimSpace(op.Summary), op.Access, r.authText(op))
	if op.Deprecated {
		line += " **Obsoleta.**"
	}
	r.printf("%s\n\n%s\n\n", line, strings.TrimSpace(op.Description))

	params, err := d.Parameters(op)
	if err != nil {
		r.fail(err)
		return
	}
	if len(params) > 0 {
		r.printf("**Parámetros**\n\n| Nombre | En | Tipo | Descripción |\n|---|---|---|---|\n")
		for _, p := range params {
			req := ""
			if p.Required {
				req = " (obligatorio)"
			}
			r.printf("| `%s` | %s%s | %s | %s |\n", p.Name, spanishIn(p.In), req, r.typeCell(p.Schema), cell(p.Description))
		}
		r.printf("\n")
	}

	if rb := op.RequestBody; rb != nil {
		need := "opcional"
		if rb.Required {
			need = "obligatorio"
		}
		r.printf("**Cuerpo** (%s): %s\n\n", need, r.contentText(rb.Content))
	}

	r.printf("**Respuestas**\n\n| Estado | Significado | Cuerpo |\n|---|---|---|\n")
	for _, code := range op.Responses.Keys {
		resp, _, err := d.Response(op.Responses.Values[code])
		if err != nil {
			r.fail(fmt.Errorf("%s %s: %w", op.Pattern(), code, err))
			continue
		}
		desc := cell(resp.Description)
		for _, h := range resp.Headers.Keys {
			desc += fmt.Sprintf(" Cabecera `%s`: %s", h, cell(resp.Headers.Values[h].Description))
		}
		body := "—"
		if resp.Content.Len() > 0 {
			body = r.contentText(resp.Content)
		}
		r.printf("| %s | %s | %s |\n", code, desc, body)
	}
	r.printf("\n")
}

func (r *renderer) authText(op Op) string {
	if op.Access == "node" {
		return "certificado de nodo (mTLS) o clave de API de plataforma"
	}
	sec := r.doc.EffectiveSecurity(op)
	if len(sec) == 0 {
		return "ninguna"
	}
	var names []string
	for _, alt := range sec {
		for scheme := range alt {
			text, ok := schemeText[scheme]
			if !ok {
				text = scheme
			}
			names = append(names, text)
		}
	}
	return strings.Join(names, " o ")
}

func spanishIn(in string) string {
	switch in {
	case "path":
		return "ruta"
	case "query":
		return "consulta"
	case "header":
		return "cabecera"
	}
	return in
}

// contentText names the schema of each content type of a body.
func (r *renderer) contentText(c Ordered[MediaType]) string {
	var parts []string
	for _, ct := range c.Keys {
		t := r.typeCell(c.Values[ct].Schema)
		if ct == "application/x-ndjson" {
			t += ", uno por línea"
		}
		if len(c.Keys) == 1 && ct == "application/json" {
			parts = append(parts, t)
		} else {
			parts = append(parts, fmt.Sprintf("`%s`: %s", ct, t))
		}
	}
	return strings.Join(parts, "; ")
}

func (r *renderer) schema(name string, s *Schema) {
	d := r.doc
	r.printf("### %s <a id=\"%s\"></a>\n\n", name, schemaAnchor(name))
	if desc := strings.TrimSpace(s.Description); desc != "" {
		r.printf("%s\n\n", desc)
	}
	if len(s.Enum) > 0 {
		r.printf("Valores: %s.\n\n", enumText(s.Enum))
		return
	}
	flat, err := d.Flatten(s)
	if err != nil {
		r.fail(fmt.Errorf("schema %s: %w", name, err))
		return
	}
	if flat.Properties.Len() == 0 {
		r.printf("%s.\n\n", capitalize(r.typeCell(s)))
		return
	}
	required := map[string]bool{}
	for _, f := range flat.Required {
		required[f] = true
	}
	r.printf("| Campo | Tipo | Obligatorio | Descripción |\n|---|---|---|---|\n")
	for _, field := range flat.Properties.Keys {
		p := flat.Properties.Values[field]
		need := "no"
		if required[field] {
			need = "sí"
		}
		r.printf("| `%s` | %s | %s | %s |\n", field, r.typeCell(p), need, cell(r.fieldDescription(p)))
	}
	r.printf("\n")
}

// fieldDescription is the text of a property: its own, or that of the schema it refers to.
func (r *renderer) fieldDescription(p *Schema) string {
	if p.Description != "" || p.Ref == "" {
		return p.Description
	}
	if target, _, err := r.doc.Deref(p); err == nil && target != nil {
		return target.Description
	}
	return ""
}

// typeCell says what a schema is, with a link to the schema it refers to.
func (r *renderer) typeCell(s *Schema) string {
	if s == nil {
		return "—"
	}
	if s.Ref != "" {
		_, name, _ := RefName(s.Ref)
		return fmt.Sprintf("[%s](#%s)", name, schemaAnchor(name))
	}
	if len(s.AllOf) > 0 {
		var parts []string
		for _, p := range s.AllOf {
			parts = append(parts, r.typeCell(p))
		}
		return strings.Join(parts, " + ")
	}
	var t string
	switch s.Type {
	case "array":
		t = "array de " + r.typeCell(s.Items)
	case "object":
		switch ap := s.AdditionalProperties; {
		case ap != nil && ap.Schema != nil:
			t = "objeto (nombre → " + r.typeCell(ap.Schema) + ")"
		case ap != nil && ap.Any && s.Properties.Len() == 0:
			t = "objeto libre"
		default:
			t = "objeto"
		}
	case "":
		t = "cualquiera"
	default:
		t = s.Type
		if s.Format != "" {
			t += " (" + s.Format + ")"
		}
	}
	if len(s.Enum) > 0 {
		t += ": " + enumText(s.Enum)
	}
	if s.Minimum != nil {
		t += fmt.Sprintf(", mínimo %g", *s.Minimum)
	}
	if s.Maximum != nil {
		t += fmt.Sprintf(", máximo %g", *s.Maximum)
	}
	if s.Nullable {
		t += ", puede ser null"
	}
	return t
}

func enumText(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("`%v`", v)
	}
	return strings.Join(parts, " \\| ")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// cell makes text safe inside a table cell.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// demoteHeadings moves every heading of a Markdown text one level down, so that a text with its own
// "## Authentication" fits under a heading of the page. Fenced code is left alone.
func demoteHeadings(md string) string {
	lines := strings.Split(md, "\n")
	fenced := false
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if strings.HasPrefix(trimmed, "```") {
			fenced = !fenced
			continue
		}
		if !fenced && strings.HasPrefix(l, "#") {
			lines[i] = "#" + l
		}
	}
	return strings.Join(lines, "\n")
}
