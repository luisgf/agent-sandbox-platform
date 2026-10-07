package main

// setting describes one variable the CLI reads. The config file may set exactly these (its
// key is the name without ASP_, in lower case) and `asp config show` lists them.
type setting struct {
	Env     string
	Default string // documentation, in words where the code decides
	Help    string
	Secret  bool
	// Legacy marks an old name that is still read (with a warning). A file cannot set it.
	Legacy bool
}

// settingsTable is every variable the CLI reads. A test fails when the source reads one that is
// not here, or this lists one the source does not read.
var settingsTable = []setting{
	{Env: "ASP_CONTROL_PLANE_URL", Default: "http://127.0.0.1:8080", Help: "base URL of the control plane"},
	{Env: "ASP_CP_URL", Help: "the old name of ASP_CONTROL_PLANE_URL", Legacy: true},
	{Env: "ASP_TENANT", Help: "tenant for create, list and run; empty means the caller's own"},
	{Env: "ASP_API_KEY", Help: "API key sent as a bearer token", Secret: true},
	{Env: "ASP_API_KEY_FILE", Help: "a file holding the API key, used when ASP_API_KEY is not set"},
	{Env: "ASP_CA_FILE", Help: "a PEM file with the certificate that signed the control plane's TLS certificate, trusted besides the system's"},
	{Env: "ASP_ID_TOKEN", Help: "IdP access token sent as a bearer token (preferred over the key)", Secret: true},
	{Env: "ASP_IDP_ACCESS_TOKEN", Help: "another name for ASP_ID_TOKEN", Secret: true},
	{Env: "ASP_REQUIRE_TOKEN", Help: "1 fails when no IdP token can be had (it fetches one when it can)"},
	{Env: "ASP_IDP_REQUIRED", Help: "the old name of ASP_REQUIRE_TOKEN", Legacy: true},
	{Env: "ASP_SESSION_DIR", Default: "~/.cache/asp/sessions", Help: "where named sessions are kept (mode 0700)"},
	{Env: "ASP_SESSION_FILE", Help: "a single session file; ignores --name"},
	{Env: "ASP_IDP_ISSUER", Help: "OIDC issuer; the token endpoint is derived from it"},
	{Env: "ASP_IDP_JWKS_URL", Help: "the IdP's key set"},
	{Env: "ASP_IDP_TOKEN_URL", Help: "OIDC token endpoint"},
	{Env: "ASP_IDP_TOKEN_CACHE", Default: "~/.cache/asp/id_token.json", Help: "where `asp auth login` keeps the token"},
	{Env: "ASP_IDP_SECRETS_FILE", Default: "~/.secrets/asp-keycloak-lab.txt", Help: "KEY=VALUE file with the IdP client and user"},
	{Env: "ASP_IDP_CLIENT_ID", Help: "OIDC client id"},
	{Env: "ASP_IDP_CLIENT_SECRET", Help: "OIDC client secret", Secret: true},
	{Env: "ASP_IDP_GRANT_TYPE", Help: "OIDC grant: password or client_credentials"},
	{Env: "ASP_IDP_USERNAME", Help: "IdP user for the password grant"},
	{Env: "ASP_IDP_USER", Help: "the short name of ASP_IDP_USERNAME"},
	{Env: "ASP_IDP_PASSWORD", Help: "IdP password for the password grant", Secret: true},
	{Env: "ASP_LOCAL_NET_APPLY", Help: "1 forces applying the local-net device, 0 never does"},
	{Env: "ASP_LOCAL_NET_OS", Help: "darwin writes the macOS utun script even on another system"},
	{Env: "ASP_LOCAL_NET_UTUN", Help: "an existing utun to reuse on macOS"},
}
