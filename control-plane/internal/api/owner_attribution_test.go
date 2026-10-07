package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Authenticated callers are attributed from their credential. The owner of a
// sandbox, and so the user_sub in the tokens minted for it, is never something
// an API key can choose (ADR-0007).
type attributionEnv struct {
	t *testing.T
	h http.Handler
}

func newAttributionEnv(t *testing.T) *attributionEnv {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	if _, err := mem.EnsureAPIKey("t1", "svc", store.APIKeyScopeTenant, "asp_tnt1", store.HashAPIKeySecret("tenant-key")); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.EnsureAPIKey("default", "ops", store.APIKeyScopePlatform, "asp_plat", store.HashAPIKeySecret("platform-key")); err != nil {
		t.Fatal(err)
	}
	return &attributionEnv{t: t, h: AuthMiddleware(mem, AuthConfig{})(testMux(NewServer(mem)))}
}

func (e *attributionEnv) do(method, path, bearer, body string, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func (e *attributionEnv) events(id, bearer string) []store.SandboxEvent {
	e.t.Helper()
	rr := e.do("GET", "/v1/sandboxes/"+id+"/events", bearer, "", nil)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("events: %d %s", rr.Code, rr.Body.String())
	}
	var out listEventsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out.Events
}

func TestAPIKeyCannotNameAnOwner(t *testing.T) {
	e := newAttributionEnv(t)
	for _, key := range []string{"tenant-key", "platform-key"} {
		for _, body := range []string{
			`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64,"owner_sub":"user:alice"}`,
			`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64,"owner_email":"alice@corp.test"}`,
		} {
			rr := e.do("POST", "/v1/sandboxes", key, body, nil)
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s with %s: status %d, want 403: %s", key, body, rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "IdP token") {
				t.Errorf("the refusal does not say where the owner comes from: %s", rr.Body.String())
			}
		}
	}
}

func TestAPIKeyIsTheActorAndOwnsNothing(t *testing.T) {
	e := newAttributionEnv(t)
	rr := e.do("POST", "/v1/sandboxes", "tenant-key", `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64}`,
		map[string]string{HeaderASPActorSub: "user:forged"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if sb.OwnerSub != "" || sb.OwnerEmail != "" {
		t.Fatalf("a sandbox created with an API key has owner %q %q", sb.OwnerSub, sb.OwnerEmail)
	}
	created := e.events(sb.ID, "tenant-key")
	if len(created) == 0 || created[0].ActorSub != "apikey:asp_tnt1" {
		t.Fatalf("create actor = %+v, want apikey:asp_tnt1 (the header must not win)", created)
	}

	// Destroy with a forged actor header and a forged body actor: still the key.
	rr = e.do("DELETE", "/v1/sandboxes/"+sb.ID, "tenant-key", "", map[string]string{HeaderASPActorSub: "user:forged"})
	if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted && rr.Code != http.StatusNoContent {
		t.Fatalf("destroy: %d %s", rr.Code, rr.Body.String())
	}
	evs := e.events(sb.ID, "tenant-key")
	last := evs[len(evs)-1]
	if last.ActorSub != "apikey:asp_tnt1" {
		t.Fatalf("destroy actor = %q, want the key", last.ActorSub)
	}
}

func TestPlatformKeyIsAttributedAsAKeyToo(t *testing.T) {
	e := newAttributionEnv(t)
	rr := e.do("POST", "/v1/sandboxes", "platform-key", `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64}`,
		map[string]string{HeaderASPActorSub: "user:operator"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	if evs := e.events(sb.ID, "platform-key"); len(evs) == 0 || evs[0].ActorSub != "apikey:asp_plat" {
		t.Fatalf("actor = %+v", evs)
	}
}
