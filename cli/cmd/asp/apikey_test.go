package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func TestAPIKeyCommands(t *testing.T) {
	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_API_KEY", "")
	exp := time.Now().Add(24 * time.Hour).UTC()
	var gotCreate client.CreateAPIKeyInput
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "create")
		_ = json.NewDecoder(r.Body).Decode(&gotCreate)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client.APIKey{ID: "k1", TenantID: "acme", Name: gotCreate.Name, Scope: "tenant", KeyPrefix: "asp_1234", ExpiresAt: &exp, Secret: "asp_secret-value-shown-once"})
	})
	mux.HandleFunc("GET /v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "list tenant="+r.URL.Query().Get("tenant_id"))
		revoked := time.Now().UTC()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []client.APIKey{
			{ID: "k1", TenantID: "acme", Name: "ci", Scope: "tenant", KeyPrefix: "asp_1234", ExpiresAt: &exp},
			{ID: "k2", TenantID: "acme", Name: "old", Scope: "tenant", KeyPrefix: "asp_5678", RevokedAt: &revoked},
		}})
	})
	mux.HandleFunc("DELETE /v1/api-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "revoke "+r.PathValue("id"))
		now := time.Now().UTC()
		_ = json.NewEncoder(w).Encode(client.APIKey{ID: r.PathValue("id"), Name: "ci", RevokedAt: &now})
	})
	mux.HandleFunc("POST /v1/api-keys/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "rotate "+r.PathValue("id"))
		_ = json.NewEncoder(w).Encode(client.APIKey{ID: r.PathValue("id"), Name: "ci", Secret: "asp_new-secret-value"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(args ...string) (int, string, string) {
		var stdout, stderr strings.Builder
		code := run(append([]string{"apikey"}, append(args, "--cp-url", srv.URL)...), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// create: the secret alone on stdout, the rest on stderr.
	code, out, errOut := do("create", "--name", "ci", "--tenant", "acme", "--ttl", "30d")
	if code != 0 || out != "asp_secret-value-shown-once\n" {
		t.Fatalf("create: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "only time") || strings.Contains(errOut, "asp_secret") {
		t.Fatalf("create notes on stderr: %q", errOut)
	}
	if gotCreate.Name != "ci" || gotCreate.TenantID != "acme" || gotCreate.TTL != "30d" {
		t.Fatalf("create sent %+v", gotCreate)
	}
	// list: a table with revoked keys marked, no secrets.
	code, out, _ = do("list", "--tenant", "acme")
	if code != 0 || !strings.Contains(out, "revoked") || !strings.Contains(out, "active") || strings.Contains(out, "secret") {
		t.Fatalf("list: exit=%d %q", code, out)
	}
	if code, out, _ = do("revoke", "k1"); code != 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %d %q", code, out)
	}
	if code, out, errOut = do("rotate", "k1"); code != 0 || out != "asp_new-secret-value\n" || !strings.Contains(errOut, "old one no longer works") {
		t.Fatalf("rotate: %d %q %q", code, out, errOut)
	}
	if strings.Join(calls, ",") != "create,list tenant=acme,revoke k1,rotate k1" {
		t.Fatalf("calls = %v", calls)
	}
	// Usage errors never reach the server.
	calls = nil
	for _, args := range [][]string{{"create"}, {"revoke"}, {"rotate", "a", "b"}, {"bogus"}, {}} {
		if code, _, _ := do(args...); code != 2 {
			t.Errorf("%v: exit=%d, want 2", args, code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a usage error reached the server: %v", calls)
	}
}
