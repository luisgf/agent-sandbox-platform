package execproxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "agent.token")
	tok, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(tok) {
		t.Fatalf("token %q is not 32 random bytes in hex", tok)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %o, want 600", fi.Mode().Perm())
	}
	again, err := LoadOrCreateToken(path)
	if err != nil || again != tok {
		t.Fatalf("second load: %q %v, want the same token", again, err)
	}
	other, err := LoadOrCreateToken(filepath.Join(t.TempDir(), "other.token"))
	if err != nil || other == tok {
		t.Fatalf("two agents got the same token: %q", other)
	}
}

func TestLoadOrCreateTokenRefusesAWeakFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.token")
	for _, content := range []string{"", "\n", "short"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateToken(path); err == nil || !strings.Contains(err.Error(), "too short") {
			t.Errorf("file %q: error %v", content, err)
		}
	}
	// An operator-provided secret, whatever its permissions, is used as is.
	if err := os.WriteFile(path, []byte("operator-chosen-secret-123\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadOrCreateToken(path); err != nil || tok != "operator-chosen-secret-123" {
		t.Fatalf("operator file: %q %v", tok, err)
	}
}

// Agents starting together on one file must end up with the same secret, and
// none may read the file before it is complete.
func TestLoadOrCreateTokenConcurrentStartsAgree(t *testing.T) {
	for round := 0; round < 50; round++ {
		path := filepath.Join(t.TempDir(), "agent.token")
		var wg sync.WaitGroup
		got := make([]string, 16)
		errs := make([]error, len(got))
		start := make(chan struct{})
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i], errs[i] = LoadOrCreateToken(path)
			}()
		}
		close(start)
		wg.Wait()
		for i, g := range got {
			if errs[i] != nil || g == "" || g != got[0] {
				t.Fatalf("round %d: agent %d got %q (%v), agent 0 got %q", round, i, g, errs[i], got[0])
			}
		}
		// No temporary file is left next to it.
		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 1 {
			t.Fatalf("round %d: %d files left in the directory", round, len(entries))
		}
	}
}

func TestRequireToken(t *testing.T) {
	var reached []string
	h := RequireToken("agent-secret-0123456789", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	do := func(method, target, auth string) int {
		req := httptest.NewRequest(method, target, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	for _, tc := range []struct {
		name, method, target, auth string
		want                       int
	}{
		{"healthz needs nothing", "GET", "/healthz", "", 200},
		{"no credential", "POST", "/v1/internal/exec", "", 401},
		{"wrong token", "POST", "/v1/internal/exec", "Bearer nope", 401},
		{"a prefix of the token", "POST", "/v1/internal/exec", "Bearer agent-secret", 401},
		{"the token plus more", "POST", "/v1/internal/exec", "Bearer agent-secret-0123456789x", 401},
		{"wrong scheme", "POST", "/v1/internal/exec", "Basic agent-secret-0123456789", 401},
		{"token in the URL", "POST", "/v1/internal/exec?token=agent-secret-0123456789", "", 401},
		{"egress check unauthenticated", "POST", "/v1/internal/egress-check", "", 401},
		{"ssh approve unauthenticated", "POST", "/v1/internal/ssh-agent/approve", "", 401},
		{"healthz is GET only", "POST", "/healthz", "", 401},
		{"right token", "POST", "/v1/internal/exec", "Bearer agent-secret-0123456789", 200},
		{"right token, stdin", "POST", "/v1/internal/exec/stdin", "Bearer agent-secret-0123456789", 200},
	} {
		if got := do(tc.method, tc.target, tc.auth); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if strings.Join(reached, ",") != "GET /healthz,POST /v1/internal/exec,POST /v1/internal/exec/stdin" {
		t.Fatalf("the wrapped handler was reached for: %v", reached)
	}
}

// The real handler behind the token: an exec for any sandbox without it never
// gets as far as the registry.
func TestLocalAPIBehindTheToken(t *testing.T) {
	s := &Server{}
	h := RequireToken("agent-secret-0123456789", s.Handler())
	for _, path := range []string{"/v1/internal/exec", "/v1/internal/exec/stdin", "/v1/internal/egress-check", "/v1/internal/ssh-agent/approve"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"sandbox_id":"victim","cmd":["id"]}`))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without the token: %d %s", path, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate challenge", path)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rr.Code)
	}
}
