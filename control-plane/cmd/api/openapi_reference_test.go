package main

import (
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/openapi"
)

// apiReferencePage is where the API reference is written, relative to the repository root.
const apiReferencePage = "docs/reference/api.md"

func apiReferenceMarkdown(t *testing.T) string {
	t.Helper()
	doc, err := openapi.Parse(api.OpenAPISpec())
	if err != nil {
		t.Fatal(err)
	}
	page, err := openapi.Markdown(doc)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestAPIReferenceIsUpToDate(t *testing.T) {
	goldenFile(t, apiReferencePage, apiReferenceMarkdown(t))
}

// The page is the document written out: an operation or a schema that is in one and not in the other
// would be a reader missing a route.
func TestAPIReferenceHasEveryOperationAndSchema(t *testing.T) {
	doc, err := openapi.Parse(api.OpenAPISpec())
	if err != nil {
		t.Fatal(err)
	}
	page := apiReferenceMarkdown(t)
	for _, op := range doc.Operations() {
		if !strings.Contains(page, "#### `"+op.Pattern()+"` <a id=\""+strings.ToLower(op.OperationID)+"\"></a>") {
			t.Errorf("the page has no section for %s", op.Pattern())
		}
	}
	for _, name := range doc.Components.Schemas.Keys {
		if !strings.Contains(page, "### "+name+" <a id=\"schema-"+strings.ToLower(name)+"\"></a>") {
			t.Errorf("the page has no section for the schema %s", name)
		}
	}
}
