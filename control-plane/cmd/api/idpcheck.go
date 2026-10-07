package main

import (
	"fmt"
	"log/slog"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
)

// EnvAllowAnyAudience lets a control plane that requires IdP tokens accept
// tokens whatever their audience.
const EnvAllowAnyAudience = "ASP_IDP_ALLOW_ANY_AUDIENCE"

// checkIdPAudience refuses to run with ASP_IDP_REQUIRED=1 and no audience. An
// IdP issues tokens to many clients; without an audience to check, a token
// minted for any other application of the same realm is a valid credential
// here. In a lab it warns.
func checkIdPAudience(cfg idp.Config) error {
	if !cfg.Enabled() || cfg.Audience != "" {
		return nil
	}
	if !cfg.Required {
		slog.Warn("idp enabled without ASP_IDP_AUDIENCE: tokens issued to any client of the issuer are accepted", "issuer", cfg.Issuer)
		return nil
	}
	if api.EnvTruthy(EnvAllowAnyAudience) {
		slog.Warn(EnvAllowAnyAudience+"=1: tokens issued to any client of the issuer are accepted", "issuer", cfg.Issuer)
		return nil
	}
	return configError{fmt.Errorf("ASP_IDP_REQUIRED=1 without ASP_IDP_AUDIENCE: a token the issuer %s minted for any other application would be accepted. "+
		"Set ASP_IDP_AUDIENCE to the audience of this API (an audience mapper on the client), or %s=1 to accept any audience", cfg.Issuer, EnvAllowAnyAudience)}
}
