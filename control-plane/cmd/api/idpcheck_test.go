package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
)

func TestCheckIdPAudience(t *testing.T) {
	const issuer = "https://auth.example/realms/asp"
	for _, tc := range []struct {
		name     string
		cfg      idp.Config
		allowAny string
		wantErr  bool
	}{
		{"idp off", idp.Config{}, "", false},
		{"required with an audience", idp.Config{Issuer: issuer, Audience: "asp-api", Required: true}, "", false},
		{"required without an audience", idp.Config{Issuer: issuer, Required: true}, "", true},
		{"required, any audience allowed", idp.Config{Issuer: issuer, Required: true}, "1", false},
		{"optional without an audience", idp.Config{Issuer: issuer}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAllowAnyAudience, tc.allowAny)
			err := checkIdPAudience(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil {
				var ce configError
				if !errors.As(err, &ce) {
					t.Fatalf("not a configError (exit 2): %T", err)
				}
				for _, want := range []string{"ASP_IDP_AUDIENCE", EnvAllowAnyAudience, issuer} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error does not mention %q: %v", want, err)
					}
				}
			}
		})
	}
}
