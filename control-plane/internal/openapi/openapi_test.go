package openapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// small is a document with one of everything the model reads.
const small = `
openapi: 3.0.3
info:
  title: Small
  version: dev
  description: |
    # Heading

    Some text.

    ` + "```" + `
    # not a heading
    ` + "```" + `
security:
  - apiKey: []
tags:
  - name: Things
    description: Things.
paths:
  /things:
    get:
      operationId: listThings
      summary: List things
      description: The things.
      tags: [Things]
      x-asp-access: tenant
      parameters:
        - $ref: '#/components/parameters/Limit'
      responses:
        '200':
          description: The things.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/ThingList'
        '401':
          $ref: '#/components/responses/Unauthorized'
    post:
      operationId: createThing
      summary: Make a thing
      description: Makes one.
      tags: [Things]
      x-asp-access: tenant
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Thing'
      responses:
        '201':
          description: Made.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Thing'
        '401':
          $ref: '#/components/responses/Unauthorized'
  /things/{id}:
    get:
      operationId: getThing
      summary: Get a thing
      description: One.
      tags: [Things]
      x-asp-access: public
      security: []
      parameters:
        - name: id
          in: path
          required: true
          description: Its id.
          schema:
            type: string
      responses:
        '200':
          description: It.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Thing'
components:
  securitySchemes:
    apiKey:
      type: http
      scheme: bearer
      description: A key.
  parameters:
    Limit:
      name: limit
      in: query
      description: How many.
      schema:
        type: integer
  responses:
    Unauthorized:
      description: No.
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Error'
  schemas:
    Error:
      type: object
      description: An error.
      required: [error]
      properties:
        error:
          type: string
          description: What.
    Base:
      type: object
      description: Common.
      required: [id]
      properties:
        id:
          type: string
          description: The id.
    Thing:
      description: A thing.
      allOf:
        - $ref: '#/components/schemas/Base'
        - type: object
          required: [name]
          properties:
            name:
              type: string
              description: Its name.
            tags:
              type: object
              description: Labels.
              additionalProperties:
                type: string
    ThingList:
      type: object
      description: Things.
      properties:
        things:
          type: array
          description: The things.
          items:
            $ref: '#/components/schemas/Thing'
`

func parse(t *testing.T, src string) *Document {
	t.Helper()
	d, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSmallDocumentIsWellFormed(t *testing.T) {
	if problems := parse(t, small).Check(); len(problems) > 0 {
		t.Errorf("problems in a good document: %v", problems)
	}
}

func TestOperationsAreInDocumentOrderWithCreateBeforeList(t *testing.T) {
	var got []string
	for _, op := range parse(t, small).Operations() {
		got = append(got, op.Pattern())
	}
	want := []string{"POST /things", "GET /things", "GET /things/{id}"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("operations = %v, want %v", got, want)
	}
}

func TestParseRefusesWhatItDoesNotKnowOrRepeats(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"a misspelled key":         func(s string) string { return strings.Replace(s, "required: [error]", "requried: [error]", 1) },
		"an unknown operation key": func(s string) string { return strings.Replace(s, "summary: List things", "sumary: List things", 1) },
		"a repeated path":          func(s string) string { return strings.Replace(s, "  /things/{id}:", "  /things:", 1) },
		"a repeated response": func(s string) string {
			return strings.Replace(s, "'401':\n          $ref: '#/components/responses/Unauthorized'\n    post:", "'200':\n          $ref: '#/components/responses/Unauthorized'\n    post:", 1)
		},
	} {
		if _, err := Parse([]byte(mutate(small))); err == nil {
			t.Errorf("%s: Parse accepted it", name)
		}
	}
}

func TestFlattenMergesReferencesAndAllOf(t *testing.T) {
	d := parse(t, small)
	thing, _ := d.Schema("Thing")
	flat, err := d.Flatten(thing)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(flat.Properties.Keys, ","); got != "id,name,tags" {
		t.Errorf("properties = %s, want id,name,tags", got)
	}
	if got := strings.Join(flat.Required, ","); got != "id,name" {
		t.Errorf("required = %s, want id,name", got)
	}
}

func TestCheckReportsWhatDoesNotHoldTogether(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(string) string
		want   string
	}{
		"a reference to nothing": {func(s string) string {
			return strings.Replace(s, "#/components/schemas/ThingList", "#/components/schemas/Nothing", 1)
		}, "no such schema"},
		"an undeclared path param": {func(s string) string {
			return strings.Replace(s, "        - name: id\n          in: path\n          required: true\n          description: Its id.\n          schema:\n            type: string\n", "", 1)
		}, "{id} is not declared"},
		"an operation without summary": {func(s string) string { return strings.Replace(s, "      summary: Make a thing\n", "", 1) }, "no summary"},
		"a duplicate operationId": {func(s string) string {
			return strings.Replace(s, "operationId: createThing", "operationId: listThings", 1)
		}, "also used by"},
		"an undeclared tag": {func(s string) string {
			return strings.Replace(s, "tags: [Things]\n      x-asp-access: public", "tags: [Other]\n      x-asp-access: public", 1)
		}, `tag "Other" is not declared`},
		"a bad access": {func(s string) string { return strings.Replace(s, "x-asp-access: public", "x-asp-access: everyone", 1) }, "x-asp-access"},
		"an orphan schema": {func(s string) string {
			return strings.Replace(s, "    Error:\n      type: object", "    Orphan:\n      type: object\n      description: x\n    Error:\n      type: object", 1)
		}, "Orphan is not used"},
		"a property with no text": {func(s string) string { return strings.Replace(s, "          description: What.\n", "", 1) }, "needs a description"},
		"no 401 on a protected route": {func(s string) string {
			return strings.Replace(s, "        '401':\n          $ref: '#/components/responses/Unauthorized'\n    post:", "    post:", 1)
		}, "answers 401"},
		"a required non-property": {func(s string) string { return strings.Replace(s, "required: [name]", "required: [name, ghost]", 1) }, `"ghost" is required`},
		"no success response":     {func(s string) string { return strings.Replace(s, "'201':", "'400':", 1) }, "no 2xx"},
	} {
		d, err := Parse([]byte(c.mutate(small)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		problems := strings.Join(d.Check(), "\n")
		if !strings.Contains(problems, c.want) {
			t.Errorf("%s: Check said %q, want it to mention %q", name, problems, c.want)
		}
	}
}

func TestJSONKeepsTheDocumentAndLetsTheCallerEditIt(t *testing.T) {
	out, err := JSON([]byte(small), func(doc map[string]any) {
		doc["info"].(map[string]any)["version"] = "9.9.9"
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OpenAPI string `json:"openapi"`
		Info    struct{ Version string }
		Paths   map[string]map[string]struct {
			OperationID string `json:"operationId"`
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.OpenAPI != "3.0.3" || got.Info.Version != "9.9.9" || got.Paths["/things"]["post"].OperationID != "createThing" {
		t.Errorf("JSON = %s", out)
	}
}

func TestMarkdownHasASectionPerOperationAndSchemaAndLinksThem(t *testing.T) {
	page, err := Markdown(parse(t, small))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#### `POST /things` <a id=\"creatething\"></a>",
		"| [`GET /things/{id}`](#getthing) | Get a thing | `public` |",
		"### Thing <a id=\"schema-thing\"></a>",
		"| `name` | string | sí | Its name. |",
		"| `id` | string | sí | The id. |",
		"| `tags` | objeto (nombre → string) | no | Labels. |",
		"[Thing](#schema-thing)",
		"Autenticación: ninguna.",
		"| 401 | No. | [Error](#schema-error) |",
		"\n## Heading", // the description's heading goes one level down
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(page, "\n## not a heading") || !strings.Contains(page, "\n# not a heading") {
		t.Error("a line inside a code block was taken for a heading")
	}
}
