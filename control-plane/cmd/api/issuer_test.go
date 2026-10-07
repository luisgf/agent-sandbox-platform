package main

import "testing"

func TestDefaultIssuer(t *testing.T) {
	for _, c := range []struct {
		addr string
		tls  bool
		want string
	}{
		{"127.0.0.1:8080", false, "http://127.0.0.1:8080"},
		{":8080", false, "http://127.0.0.1:8080"},
		{"0.0.0.0:8080", false, "http://127.0.0.1:8080"},
		{"[::]:8080", false, "http://127.0.0.1:8080"},
		{"cp.example.corp:8443", true, "https://cp.example.corp:8443"},
		{"[fd00::1]:8443", true, "https://[fd00::1]:8443"},
		{"localhost:18112", false, "http://localhost:18112"},
	} {
		if got := defaultIssuer(c.addr, c.tls); got != c.want {
			t.Errorf("defaultIssuer(%q, tls=%v) = %q, want %q", c.addr, c.tls, got, c.want)
		}
	}
}
