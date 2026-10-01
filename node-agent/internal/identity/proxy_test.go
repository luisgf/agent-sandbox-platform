package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTokenRequiresAud(t *testing.T) {
	p := &Proxy{ControlPlaneURL: "http://127.0.0.1:9", DefaultSandboxID: "sb"}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tokens/oidc", strings.NewReader(`{}`))
	p.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTokenForwardsToCP(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/oidc/token" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["sandbox_id"] != "sb-auth" || body["aud"] != "https://api" {
			t.Errorf("mint body=%v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok", "token_type": "Bearer", "expires_in": 300,
		})
	}))
	defer cp.Close()

	p := &Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client(), DefaultSandboxID: "sb-auth"}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tokens/oidc",
		strings.NewReader(`{"aud":"https://api","sandbox_id":"evil-override"}`))
	p.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out mintResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.AccessToken != "tok" {
		t.Fatalf("out=%+v", out)
	}
}

func TestTokenIgnoresGuestUserSubAndAct(t *testing.T) {
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["sandbox_id"] != "sb-auth" {
			t.Errorf("sandbox_id=%v", body["sandbox_id"])
		}
		// Proxy must not forward guest user_sub/act to CP.
		if _, ok := body["user_sub"]; ok {
			t.Errorf("proxy forwarded user_sub: %v", body["user_sub"])
		}
		if _, ok := body["act"]; ok {
			t.Errorf("proxy forwarded act: %v", body["act"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok", "token_type": "Bearer", "expires_in": 300,
		})
	}))
	defer cp.Close()

	p := &Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client(), DefaultSandboxID: "sb-auth"}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/tokens/oidc",
		strings.NewReader(`{"aud":"https://api","user_sub":"user:eve","act":{"sub":"user:eve"}}`))
	p.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
