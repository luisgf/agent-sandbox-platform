package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func testIdP(t *testing.T) (*rsa.PrivateKey, string, *idp.Validator) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const iss = "https://idp.test.local"
	const aud = "asp-api"
	const kid = "lab-kid"
	v, err := idp.NewValidatorWithPublicKeys(idp.Config{Issuer: iss, Audience: aud}, map[string]*rsa.PublicKey{
		kid: &key.PublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return key, kid, v
}

func mintUserJWT(t *testing.T, key *rsa.PrivateKey, kid, sub, email string) string {
	t.Helper()
	return mintUserJWTWithGroups(t, key, kid, sub, email, []string{"asp-operator"})
}

func mintUserJWTWithGroups(t *testing.T, key *rsa.PrivateKey, kid, sub, email string, groups []string) string {
	t.Helper()
	extra := map[string]any{}
	if len(groups) > 0 {
		extra["groups"] = groups
	}
	tok, err := idp.SignTestTokenClaims(key, kid, "https://idp.test.local", sub, "asp-api", email, time.Now().Add(time.Hour), extra)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestIdPCreateSetsOwnerFromToken(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	tok := mintUserJWT(t, key, kid, "user:alice", "alice@corp.test")
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if sb.OwnerSub != "user:alice" || sb.OwnerEmail != "alice@corp.test" {
		t.Fatalf("owner=%+v", sb)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var ev listEventsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &ev)
	if len(ev.Events) < 1 || ev.Events[0].ActorSub != "user:alice" {
		t.Fatalf("events=%+v", ev.Events)
	}
}

func TestIdPRejectsForgedOwnerSub(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	tok := mintUserJWT(t, key, kid, "user:alice", "")
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128,"owner_sub":"user:eve"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestIdPInvalidTokenUnauthorized(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	_, _, v := testIdP(t)
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.fakesig")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestIdPRequiredRejectsWithoutJWT(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v, IdPRequired: true})(testMux(srv))

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without jwt, got %d", rr.Code)
	}

	tok := mintUserJWT(t, key, kid, "user:bob", "")
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201 with jwt, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestIdPOffKeepsLabBodyOwner(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t)
	srv := NewServer(mem)
	// No IdP configured — phase 1 lab behavior.
	h := AuthMiddleware(mem, AuthConfig{})(testMux(srv))
	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128,"owner_sub":"user:lab"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d", rr.Code)
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	if sb.OwnerSub != "user:lab" {
		t.Fatalf("owner=%q", sb.OwnerSub)
	}
}

func TestIdPDoesNotBreakNodeWorkWithoutJWT(t *testing.T) {
	mem := store.NewMemoryStore()
	srv := NewServer(mem)
	_, err := mem.RegisterNode(store.RegisterNodeInput{
		ID: "n1", Name: "n1", AgentEndpoint: "http://127.0.0.1:9",
		CapacityCPU: 1, CapacityMemMiB: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	key, _, v := testIdP(t)
	_ = key
	h := AuthMiddleware(mem, AuthConfig{IdP: v, IdPRequired: true})(testMux(srv))

	// Node work is not user-facing IdP-required; with no API keys and Require=false, open lab.
	req := httptest.NewRequest(http.MethodGet, "/v1/nodes/n1/work", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("node work want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// Claim path is node-agent, not IdP-required.
	t.Setenv("ASP_AUTO_PROVISION", "0")
	// create still needs JWT when required
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/x/claim", bytes.NewBufferString(`{}`))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// 404/conflict whatever — must not be 401 idp
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("claim must not require IdP JWT: %s", rr.Body.String())
	}
}

func TestIdPActorOverridesHeaderOnExecDestroy(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))
	tok := mintUserJWT(t, key, kid, "user:alice", "")

	body := `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(HeaderASPActorSub, "user:forged")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("destroy=%d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var ev listEventsResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &ev)
	last := ev.Events[len(ev.Events)-1]
	if last.ActorSub != "user:alice" {
		t.Fatalf("token actor must win over header, got %q", last.ActorSub)
	}
}
