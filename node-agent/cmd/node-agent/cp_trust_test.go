package main

import (
	"strings"
	"testing"
)

func TestControlPlaneTrustWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		ca   string
		have bool
		warn bool
	}{
		{"remote https, enrollment CA only", "https://cp.example.com:8443", "", true, true},
		{"remote https by address", "https://203.0.113.9:8443", "", true, true},
		{"explicit control plane CA", "https://cp.example.com:8443", "/etc/asp/cp-ca.pem", true, false},
		{"loopback https", "https://127.0.0.1:8443", "", true, false},
		{"localhost https", "https://localhost:8443", "", true, false},
		{"plain http", "http://cp.example.com:8080", "", true, false},
		{"no enrollment CA in use", "https://cp.example.com:8443", "", false, false},
		{"unparsable url", "https://[::1", "", true, false},
	} {
		got := controlPlaneTrustWarning(config{ControlPlaneURL: tc.url, ControlPlaneCA: tc.ca}, tc.have)
		if (got != "") != tc.warn {
			t.Errorf("%s: warning %q, want warning=%v", tc.name, got, tc.warn)
		}
		if tc.warn && !strings.Contains(got, "--control-plane-ca") {
			t.Errorf("%s: the warning does not say what to do: %q", tc.name, got)
		}
	}
}
