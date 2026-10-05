package identity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// guestRequest is a token request on a connection bound to sandboxID ("" =
// unbound), naming header in X-ASP-Sandbox-ID unless empty.
func guestRequest(sandboxID, header, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/tokens/oidc", strings.NewReader(body))
	if header != "" {
		req.Header.Set(SandboxHeader, header)
	}
	if sandboxID != "" {
		req = req.WithContext(WithSandboxID(req.Context(), sandboxID))
	}
	return req
}

// mintRecorder is a control plane that records the sandbox of every mint.
func mintRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body mintRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.SandboxID)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(mintResponse{AccessToken: "tok-" + body.SandboxID, TokenType: "Bearer", ExpiresIn: 300})
	}))
	t.Cleanup(cp.Close)
	return cp, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestTokenRequiresAud(t *testing.T) {
	p := &Proxy{ControlPlaneURL: "http://127.0.0.1:9"}
	rr := httptest.NewRecorder()
	p.Handler().ServeHTTP(rr, guestRequest("sb", "", `{}`))
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

	p := &Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client()}
	rr := httptest.NewRecorder()
	p.Handler().ServeHTTP(rr, guestRequest("sb-auth", "", `{"aud":"https://api","sandbox_id":"evil-override"}`))
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

	p := &Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client()}
	rr := httptest.NewRecorder()
	p.Handler().ServeHTTP(rr, guestRequest("sb-auth", "", `{"aud":"https://api","user_sub":"user:eve","act":{"sub":"user:eve"}}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTokenSandboxComesFromTheConnection(t *testing.T) {
	cp, minted := mintRecorder(t)
	for _, tc := range []struct {
		name          string
		bound, header string
		trust         bool   // TrustSandboxHeader (lab flag)
		def           string // DefaultSandboxID
		want          int
		minted        string // sandbox the control plane is asked for; "" = not called
	}{
		{name: "bound", bound: "sb-a", want: http.StatusOK, minted: "sb-a"},
		{name: "bound, own header", bound: "sb-a", header: "sb-a", want: http.StatusOK, minted: "sb-a"},
		{name: "bound, other header", bound: "sb-a", header: "sb-b", want: http.StatusForbidden},
		{name: "bound, other header, lab flag", bound: "sb-a", header: "sb-b", trust: true, want: http.StatusForbidden},
		{name: "bound beats default", bound: "sb-a", trust: true, def: "sb-b", want: http.StatusOK, minted: "sb-a"},
		{name: "unbound, header", header: "sb-b", want: http.StatusForbidden},
		{name: "unbound, default", def: "sb-a", want: http.StatusForbidden},
		{name: "unbound, lab flag, header", header: "sb-b", trust: true, def: "sb-a", want: http.StatusOK, minted: "sb-b"},
		{name: "unbound, lab flag, default", trust: true, def: "sb-a", want: http.StatusOK, minted: "sb-a"},
		{name: "unbound, lab flag, no sandbox", trust: true, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{ControlPlaneURL: cp.URL, HTTP: cp.Client(), TrustSandboxHeader: tc.trust, DefaultSandboxID: tc.def}
			before := len(minted())
			rr := httptest.NewRecorder()
			p.Handler().ServeHTTP(rr, guestRequest(tc.bound, tc.header, `{"aud":"https://api"}`))
			if rr.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rr.Code, tc.want, rr.Body.String())
			}
			want := "[]"
			if tc.minted != "" {
				want = "[" + tc.minted + "]"
			}
			if got := fmt.Sprint(minted()[before:]); got != want {
				t.Fatalf("control plane minted %s, want %s", got, want)
			}
		})
	}
}

func TestTokenRefusesAnyOtherSandboxHeader(t *testing.T) {
	p := &Proxy{ControlPlaneURL: "http://127.0.0.1:9"}
	req := guestRequest("sb-a", "sb-a", `{"aud":"https://api"}`)
	req.Header.Add(SandboxHeader, "sb-b")
	rr := httptest.NewRecorder()
	p.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}
