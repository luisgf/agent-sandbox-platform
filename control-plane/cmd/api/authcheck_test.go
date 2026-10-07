package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestCheckAuthConfigured(t *testing.T) {
	withKey := store.NewMemoryStore()
	if _, err := withKey.EnsureAPIKey("default", "k", store.APIKeyScopePlatform, "asp_k", store.HashAPIKeySecret("secret-secret-secret")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		st      store.Store
		idp     bool
		cfg     api.AuthConfig
		wantErr bool
	}{
		{"no key, no IdP", store.NewMemoryStore(), false, api.AuthConfig{}, true},
		{"a key exists", withKey, false, api.AuthConfig{}, false},
		{"an IdP is configured", store.NewMemoryStore(), true, api.AuthConfig{}, false},
		{"open on purpose", store.NewMemoryStore(), false, api.AuthConfig{InsecureOpen: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAuthConfigured(tc.st, tc.idp, tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				var ce configError
				if !errors.As(err, &ce) {
					t.Fatalf("not a configError (exit 2): %T", err)
				}
				for _, want := range []string{"ASP_BOOTSTRAP_API_KEY", "ASP_IDP_ISSUER", api.EnvInsecureOpenAPI} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the error does not offer %s: %v", want, err)
					}
				}
			}
		})
	}
}
