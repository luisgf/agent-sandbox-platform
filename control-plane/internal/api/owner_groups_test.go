package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// ADR-0020: the workload token of a sandbox with an owner carries the owner's groups, as the IdP
// token that created it spelled them; nobody on the guest's side can change them.

func createWithIdP(t *testing.T, h http.Handler, tok, body string) store.Sandbox {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

// mintToken mints through the node route and returns the JWT, its verified claims and the
// convenience claims of the response.
func mintToken(t *testing.T, mux http.Handler, body string) (string, oidc.Claims, map[string]any) {
	t.Helper()
	jwksRR := httptest.NewRecorder()
	mux.ServeHTTP(jwksRR, httptest.NewRequest(http.MethodGet, "/oidc/jwks.json", nil))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/oidc/token", bytes.NewBufferString(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint=%d %s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &mint); err != nil {
		t.Fatal(err)
	}
	token, _ := mint["access_token"].(string)
	claims, err := oidc.VerifyAgainstJWKS(token, jwksRR.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	out, _ := mint["claims"].(map[string]any)
	return token, claims, out
}

// wireClaims decodes the payload of a JWT as the consumer sees it.
func wireClaims(t *testing.T, token string) map[string]any {
	t.Helper()
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWorkloadTokenCarriesTheOwnersGroups(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	srv := NewServer(mem)
	oidcKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv.OIDC = oidc.NewSignerFromKey(oidcKey, "http://issuer.test")
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))
	mux := testMux(srv) // the node route, as a node calls it

	groups := []string{"asp-operator", "SRE", "db-admins"}
	tok := mintUserJWTWithGroups(t, key, kid, "user:alice", "alice@corp.test", groups)
	// owner_groups in the body is not a field: it changes nothing.
	sb := createWithIdP(t, h, tok, `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128,"owner_groups":["root"]}`)
	if !reflect.DeepEqual(sb.OwnerGroups, groups) {
		t.Fatalf("owner_groups=%v want %v", sb.OwnerGroups, groups)
	}

	// A guest that asks for other groups (or a user) gets the sandbox's.
	body := `{"sandbox_id":"` + sb.ID + `","aud":"infrabroker","groups":["root"],"user_sub":"user:eve"}`
	token, claims, out := mintToken(t, mux, body)
	if !reflect.DeepEqual(claims.Groups, groups) {
		t.Fatalf("jwt groups=%v want %v", claims.Groups, groups)
	}
	if claims.UserSub != "user:alice" || claims.Subject != "sandbox/"+sb.ID {
		t.Fatalf("claims=%+v", claims)
	}
	wire := wireClaims(t, token)
	if got, _ := wire["groups"].([]any); len(got) != len(groups) || got[1] != "SRE" {
		t.Fatalf("wire groups=%v", wire["groups"])
	}
	if got, _ := out["groups"].([]any); len(got) != len(groups) {
		t.Fatalf("response claims groups=%v", out["groups"])
	}

	// The same sandbox read back from the store says the same (the row, not the create's echo).
	got, err := mem.GetSandbox(t.Context(), sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.OwnerGroups, groups) {
		t.Fatalf("stored owner_groups=%v", got.OwnerGroups)
	}
}

func TestWorkloadTokenWithoutOwnerHasNoGroupsClaim(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t)
	srv := NewServer(mem)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv.OIDC = oidc.NewSignerFromKey(key, "http://issuer.test")
	mux := testMux(srv)

	cases := map[string]string{
		"open lab, no owner":                  `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64,"owner_groups":["root"]}`,
		"open lab, owner named, groups named": `{"tenant_id":"t1","image_ref":"img","cpu_millis":1,"memory_mib":64,"owner_sub":"user:alice","owner_groups":["root"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body)))
			if rr.Code != http.StatusCreated {
				t.Fatalf("create=%d %s", rr.Code, rr.Body.String())
			}
			var sb store.Sandbox
			_ = json.Unmarshal(rr.Body.Bytes(), &sb)
			if sb.OwnerGroups != nil {
				t.Fatalf("owner_groups=%v: the body cannot name them", sb.OwnerGroups)
			}
			token, claims, out := mintToken(t, mux, `{"sandbox_id":"`+sb.ID+`","aud":"infrabroker","groups":["root"]}`)
			if claims.Groups != nil {
				t.Fatalf("jwt groups=%v", claims.Groups)
			}
			if _, present := wireClaims(t, token)["groups"]; present {
				t.Fatalf("the claim must be absent, not empty: %s", token)
			}
			if _, present := out["groups"]; present {
				t.Fatalf("response claims=%v", out)
			}
		})
	}
}

func TestIdPCreateRefusesMoreGroupsThanATokenCarries(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	srv := NewServer(mem)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(srv))

	groups := []string{"asp-operator"}
	for i := 0; i < store.MaxOwnerGroups; i++ {
		groups = append(groups, fmt.Sprintf("team-%d", i))
	}
	tok := mintUserJWTWithGroups(t, key, kid, "user:alice", "", groups)
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "groups") {
		t.Fatalf("want 400 naming the groups, got %d %s", rr.Code, rr.Body.String())
	}
	// Repeats and blanks do not count: the same token with them fits.
	fits := append([]string{" asp-operator ", ""}, groups[:store.MaxOwnerGroups]...)
	tok = mintUserJWTWithGroups(t, key, kid, "user:alice", "", append(fits, "asp-operator"))
	sb := createWithIdP(t, h, tok, `{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":128}`)
	if len(sb.OwnerGroups) != store.MaxOwnerGroups || sb.OwnerGroups[0] != "asp-operator" {
		t.Fatalf("owner_groups=%d %v", len(sb.OwnerGroups), sb.OwnerGroups[:2])
	}
}
