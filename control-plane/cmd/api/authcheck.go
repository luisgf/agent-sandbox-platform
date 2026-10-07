package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/api"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// checkAuthConfigured refuses to start a control plane nobody could ever
// authenticate to, and says so loudly when the operator turns authentication
// off on purpose.
//
// Authentication is always on. Without a key in the store or an IdP, every
// request would be refused, which is not a deployment, it is a mistake: better
// to stop with the three ways out than to answer 401 to everything.
func checkAuthConfigured(st store.Store, idpOn bool, cfg api.AuthConfig) error {
	if os.Getenv("ASP_REQUIRE_API_KEY") != "" {
		slog.Warn("ASP_REQUIRE_API_KEY is no longer used: authentication is always on")
	}
	if cfg.InsecureOpen {
		slog.Warn(api.EnvInsecureOpenAPI + "=1: the API accepts requests with no credential. " +
			"Anyone who can reach it can create sandboxes and run commands in them. For a lab or a laptop only")
		return nil
	}
	n, err := st.CountAPIKeys()
	if err != nil {
		return fmt.Errorf("count api keys: %w", err)
	}
	if n == 0 && !idpOn {
		return configError{fmt.Errorf("no way to authenticate: no API key exists and no IdP is configured, so every request would be refused. "+
			"Set ASP_BOOTSTRAP_API_KEY to create the first key, or ASP_IDP_ISSUER to use IdP tokens, "+
			"or %s=1 to run a lab with no authentication", api.EnvInsecureOpenAPI)}
	}
	return nil
}
