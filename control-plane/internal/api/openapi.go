package api

import (
	_ "embed"
	"net/http"
	"sync"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/openapi"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/version"
)

// openapiYAML is the OpenAPI document of this API, written by hand. Tests keep it in step with the routes
// and the types of the handlers (openapi_test.go); docs/reference/api.md is generated from it.
//
//go:embed openapi.yaml
var openapiYAML []byte

// OpenAPISpec returns the OpenAPI document as it is written (YAML).
func OpenAPISpec() []byte { return openapiYAML }

var (
	openapiOnce sync.Once
	openapiJSON []byte
	openapiErr  error
)

// GetOpenAPI serves GET /openapi.json: the OpenAPI document as JSON, with the version of this build as
// info.version. It needs no credential: it says what the API is, and nothing about a deployment.
func (s *Server) GetOpenAPI(w http.ResponseWriter, _ *http.Request) {
	openapiOnce.Do(func() {
		openapiJSON, openapiErr = openapi.JSON(openapiYAML, func(doc map[string]any) {
			if info, ok := doc["info"].(map[string]any); ok {
				info["version"] = version.Short()
			}
		})
	})
	if openapiErr != nil {
		writeError(w, http.StatusInternalServerError, "openapi: "+openapiErr.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(openapiJSON, '\n'))
}
