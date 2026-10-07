package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A flag default is printed in the usage text. With the credentials taken from
// the environment that would put an API key or a token in the output of
// `asp -h`, which agent harnesses log.
func TestUsageDoesNotPrintSecrets(t *testing.T) {
	t.Setenv("ASP_API_KEY", "sk-secret-api-key")
	t.Setenv("ASP_ID_TOKEN", "eyJ-secret-id-token")
	t.Setenv("ASP_IDP_ACCESS_TOKEN", "eyJ-secret-access-token")
	for _, args := range [][]string{
		{"sandbox", "list", "-h"},
		{"sandbox", "get", "-h"},
		{"session", "start", "-h"},
		{"node", "list", "-h"},
		{"sandbox", "list", "--no-such-flag"},
	} {
		var stdout, stderr strings.Builder
		run(args, &stdout, &stderr)
		out := stdout.String() + stderr.String()
		if strings.Contains(out, "secret") {
			t.Errorf("asp %s printed a credential:\n%s", strings.Join(args, " "), out)
		}
		if !strings.Contains(out, "-api-key") && !strings.Contains(out, "--api-key") && strings.Contains(strings.Join(args, " "), "-h") {
			t.Errorf("asp %s lost the --api-key flag from its usage:\n%s", strings.Join(args, " "), out)
		}
	}
}

// With no flag the credentials still come from the environment.
func TestCredentialsFromEnvironmentStillWork(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("ASP_REQUIRE_TOKEN", "")

	t.Setenv("ASP_IDP_REQUIRED", "")
	t.Setenv("ASP_ID_TOKEN", "")
	t.Setenv("ASP_IDP_ACCESS_TOKEN", "")
	t.Setenv("ASP_API_KEY", "key-from-env")
	var stdout, stderr strings.Builder
	if code := run([]string{"sandbox", "list", "--control-plane-url", srv.URL, "--tenant", "default"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if gotAuth != "Bearer key-from-env" {
		t.Fatalf("API key from the environment not sent: %q", gotAuth)
	}

	t.Setenv("ASP_ID_TOKEN", "jwt-from-env")
	if code := run([]string{"sandbox", "list", "--control-plane-url", srv.URL, "--tenant", "default"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if gotAuth != "Bearer jwt-from-env" {
		t.Fatalf("ID token from the environment not sent: %q", gotAuth)
	}
}
