package fence

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestHTTPWebhookFence(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &HTTPWebhook{Client: srv.Client()}
	err := p.Fence(context.Background(), Target{
		NodeID:   "dead-node",
		Endpoint: srv.URL,
		Token:    "secret-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("auth=%q", gotAuth)
	}
	if gotBody["action"] != "power_off" || gotBody["node_id"] != "dead-node" {
		t.Fatalf("body=%v", gotBody)
	}
}

func TestHTTPWebhookDeny(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	p := &HTTPWebhook{Client: srv.Client()}
	if err := p.Fence(context.Background(), Target{Endpoint: srv.URL}); err == nil {
		t.Fatal("expected error")
	}
}

func TestRedfishStub(t *testing.T) {
	var path string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		u, p, ok := r.BasicAuth()
		if ok {
			auth = u + ":" + p
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := &Redfish{Client: srv.Client()}
	err := p.Fence(context.Background(), Target{
		Endpoint: srv.URL,
		Token:    "bmc-pass",
		User:     "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset") {
		t.Fatalf("path=%q", path)
	}
	if auth != "root:bmc-pass" {
		t.Fatalf("auth=%q", auth)
	}
}

func TestIPMISoftFail(t *testing.T) {
	p := &IPMI{
		LookPath: func(string) (string, error) {
			return "", exec.ErrNotFound
		},
	}
	err := p.Fence(context.Background(), Target{Endpoint: "10.0.0.1"})
	var sf SoftFailError
	if err == nil {
		t.Fatal("expected soft-fail")
	}
	if !errorsAs(err, &sf) && !strings.Contains(err.Error(), "soft-fail") {
		t.Fatalf("got %v", err)
	}
}

func TestNoop(t *testing.T) {
	if err := (Noop{}).Fence(context.Background(), Target{}); err != nil {
		t.Fatal(err)
	}
}

func errorsAs(err error, target *SoftFailError) bool {
	if e, ok := err.(SoftFailError); ok {
		*target = e
		return true
	}
	return false
}

// A provider name that is none of them must not leave fencing "on" with a provider that does nothing.
func TestAnUnknownProviderIsRefusedNotIgnored(t *testing.T) {
	for _, tc := range []struct {
		value   string
		valid   bool
		enabled bool
	}{
		{"", true, false}, {"noop", true, false}, {"none", true, false}, {"NOOP", true, false},
		{"http_webhook", true, true}, {"webhook", true, true}, {"redfish", true, true}, {"IPMI", true, true},
		{"redfsh", false, false}, {"ssh", false, false},
	} {
		t.Setenv("ASP_FENCE_PROVIDER", tc.value)
		if got := Validate() == nil; got != tc.valid {
			t.Errorf("%q: valid=%v, want %v", tc.value, got, tc.valid)
		}
		if got := Enabled(); got != tc.enabled {
			t.Errorf("%q: Enabled=%v, want %v", tc.value, got, tc.enabled)
		}
	}
}
