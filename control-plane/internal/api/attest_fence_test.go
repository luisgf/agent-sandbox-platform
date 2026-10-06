package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestAttestStoreGetVerifyAndOIDCClaim(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "1")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	att := attest.NewSoftwareAttestorFromKey(key)
	st := store.NewMemoryStore()
	srv := NewServer(st)
	srv.Attestor = att
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv.OIDC = oidc.NewSignerFromKey(rsaKey, "http://issuer.test")
	mux := testMux(srv)

	body := `{"tenant_id":"t1","image_ref":"debian:bookworm","cpu_millis":1000,"memory_mib":512}`
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)

	stmt := attest.BootStatement{
		SandboxID:   sb.ID,
		ImageDigest: "sha256:deadbeef",
		VMMProfile:  "cloud-hypervisor",
		CID:         5,
		NodeID:      "node-a",
		TS:          time.Now().UTC().Format(time.RFC3339),
	}
	ev, err := att.Attest(context.Background(), stmt)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(ev)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/attest", bytes.NewReader(payload)))
	if rr.Code != http.StatusOK {
		t.Fatalf("attest %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+sb.ID+"/attestation", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("get attestation %d", rr.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["fresh"] != true {
		t.Fatalf("want fresh: %v", got)
	}

	verifyBody, _ := json.Marshal(map[string]any{"evidence": ev})
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/attestation/verify", bytes.NewReader(verifyBody)))
	if rr.Code != http.StatusOK {
		t.Fatalf("verify %d", rr.Code)
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got["valid"] != true {
		t.Fatalf("verify=%v", got)
	}

	mintBody, _ := json.Marshal(map[string]string{"sandbox_id": sb.ID, "aud": "https://api.example"})
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/internal/oidc/token", bytes.NewReader(mintBody)))
	if rr.Code != http.StatusOK {
		t.Fatalf("mint %d %s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mint)
	claims := mint["claims"].(map[string]any)
	if claims["x_asp_attestation"] == nil {
		t.Fatalf("missing x_asp_attestation in %v", claims)
	}
}
