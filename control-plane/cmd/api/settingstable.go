package main

// setting describes one variable the control plane reads: what it is for, what it is when
// nothing sets it, and whether it is a credential. The configuration file may set exactly
// these (its key is the name without ASP_, in lower case), and --print-config lists them.
type setting struct {
	Env     string
	Default string // documentation: where it is not obvious from the code, in words
	Help    string
	Secret  bool
}

// settingsTable is every variable the control plane reads. A test fails when the source reads
// one that is not here, or this lists one the source does not read.
var settingsTable = []setting{
	// Where it listens, and how it stops.
	{Env: "ASP_LISTEN_ADDR", Default: "127.0.0.1:8080", Help: "address the API listens on"},
	{Env: "ASP_SHUTDOWN_TIMEOUT", Default: "30s", Help: "how long a SIGTERM waits for requests in flight"},
	{Env: "ASP_BUFFERED_EXEC_TIMEOUT", Default: "10m", Help: "how long an exec without streaming may take"},
	{Env: "ASP_METRICS_LISTEN", Help: "host:port of a listener of its own for GET /metrics (loopback unless ASP_INSECURE_OBS_LISTEN)"},
	{Env: "ASP_PPROF_LISTEN", Help: "host:port of a listener of its own for /debug/pprof/ (loopback unless ASP_INSECURE_OBS_LISTEN)"},
	{Env: "ASP_INSECURE_OBS_LISTEN", Help: "1 lets the metrics and pprof listeners listen outside the loopback (they have no authentication)"},

	// The database.
	{Env: "ASP_DATABASE_URL", Help: "Postgres connection URL; without it the state is in memory", Secret: true},
	{Env: "ASP_DB_STATEMENT_TIMEOUT", Default: "30s", Help: "server-side limit for any one statement"},
	{Env: "ASP_DB_LOCK_TIMEOUT", Default: "10s", Help: "server-side limit for waiting on a lock"},
	{Env: "ASP_DB_IDLE_TX_TIMEOUT", Default: "60s", Help: "server-side limit for an idle transaction"},

	// Who may call it.
	{Env: "ASP_INSECURE_OPEN_API", Help: "1 accepts requests with no credential (labs and dry-run smokes only)"},
	{Env: "ASP_BOOTSTRAP_API_KEY", Help: "the first API key, with platform scope unless ASP_BOOTSTRAP_API_KEY_SCOPE says otherwise", Secret: true},
	{Env: "ASP_BOOTSTRAP_API_KEY_SCOPE", Default: "platform", Help: "scope of the bootstrap key: platform or tenant"},
	{Env: "ASP_BOOTSTRAP_API_KEY_TENANT", Default: "default", Help: "tenant of the bootstrap key"},
	{Env: "ASP_DEFAULT_TENANT", Default: "default", Help: "tenant of a create with no tenant from a caller that has none"},
	{Env: "ASP_REQUIRE_API_KEY", Help: "no longer does anything: authentication is always on"},
	{Env: "ASP_IDP_ISSUER", Help: "OIDC issuer of the corporate IdP; empty means no IdP"},
	{Env: "ASP_IDP_AUDIENCE", Help: "audience the IdP's tokens must carry (required with ASP_IDP_REQUIRED)"},
	{Env: "ASP_IDP_ALLOW_ANY_AUDIENCE", Help: "1 allows ASP_IDP_REQUIRED without an audience (lab only)"},
	{Env: "ASP_IDP_JWKS_URL", Help: "the IdP's key set; derived from the issuer when empty"},
	{Env: "ASP_IDP_REQUIRE_EXP", Default: "1", Help: "0 accepts tokens with no expiry (not recommended)"},
	{Env: "ASP_IDP_REQUIRED", Default: "0", Help: "1 requires an IdP token on the sandbox routes"},
	{Env: "ASP_IDP_ROLE_CLAIM", Default: "groups", Help: "claim that holds the user's groups or roles"},
	{Env: "ASP_IDP_ROLE_MAP", Help: "CSV claim:role (admin, operator, user, viewer); wins over the prefix"},
	{Env: "ASP_IDP_ROLE_PREFIX", Default: "asp-", Help: "prefix that makes a group a role (asp-admin, asp-operator, ...)"},
	{Env: "ASP_IDP_DESTROY_ANY_GROUP", Default: "sandbox:destroy-any", Help: "group that lets an operator destroy sandboxes that are not theirs"},
	{Env: "ASP_IDP_EXEC_ANY_GROUP", Default: "sandbox:exec-any", Help: "group that lets an operator exec in sandboxes that are not theirs"},
	{Env: "ASP_IDP_TENANT_CLAIM", Default: "tenant_id", Help: "claim that holds the user's tenant"},
	{Env: "ASP_IDP_DEFAULT_TENANT", Help: "tenant of a token that has no tenant claim (single-tenant setups)"},

	// Nodes.
	{Env: "ASP_NODE_BOOTSTRAP_TOKEN", Help: "shared secret a node enrolls with (single-use enroll tokens are better)", Secret: true},
	{Env: "ASP_CA_CERT", Default: "/tmp/asp-dev-ca/ca.crt", Help: "enrollment CA certificate (created if missing); keep it outside /tmp"},
	{Env: "ASP_CA_KEY", Default: "/tmp/asp-dev-ca/ca.key", Help: "enrollment CA key (created if missing); keep it outside /tmp"},
	{Env: "ASP_ALLOW_TMP_KEYS", Help: "1 starts in production mode with keys in a temporary directory"},
	{Env: "ASP_TLS_CERT", Help: "TLS certificate of the API"},
	{Env: "ASP_TLS_KEY", Help: "TLS key of the API"},
	{Env: "ASP_CLIENT_CA", Help: "CA that vouches for node certificates; with it each route of a node needs that node's certificate"},
	{Env: "ASP_MTLS_STRICT", Help: "1 requires a client certificate on the TLS listener"},
	{Env: "ASP_ENROLL_LISTEN", Default: "127.0.0.1:8081", Help: "plaintext enroll-only listener when ASP_MTLS_STRICT is on"},
	{Env: "ASP_AGENT_TOKEN_FILE", Default: "/var/lib/asp/agent.token", Help: "secret of the node-agent on this host, sent as bearer with each exec"},
	{Env: "ASP_INSECURE_AGENT_HTTP", Help: "1 allows http:// agent endpoints outside the loopback (exec unauthenticated; lab only)"},
	{Env: "ASP_WORKSPACE_ROOTS", Help: "CSV of workspace roots (<root>/<tenant>/...); a workspace outside is a 400 at create"},
	{Env: "ASP_LOCAL_NET_DIAL", Help: "host[:port] laptops dial for local-net tunnels when the node does not say"},

	// Scheduling and liveness.
	{Env: "ASP_SCHED_POLICY", Default: "spread", Help: "spread or binpack"},
	{Env: "ASP_SCHED_CPU_OVERCOMMIT", Default: "4", Help: "vCPUs per physical core; memory is never overcommitted"},
	{Env: "ASP_SCHED_VM_OVERHEAD_MIB", Default: "64", Help: "memory a microVM costs besides its memory_mib"},
	{Env: "ASP_NODE_STALE_AFTER", Default: "90s", Help: "a node silent this long gets no sandboxes and is marked offline"},
	{Env: "ASP_NODE_MONITOR_INTERVAL", Default: "15s", Help: "how often the monitor checks the nodes"},
	{Env: "ASP_NODE_FAILOVER_AFTER", Default: "5m", Help: "a node silent this long is fenced and its sandboxes fail (0 or off: only revoked nodes)"},
	{Env: "ASP_AUTO_PROVISION", Help: "1 places sandboxes on a stub node with no agent (dev only)"},

	// Keys of the identity and attestation features.
	{Env: "ASP_OIDC_KEY", Default: "/tmp/asp-oidc-key.pem", Help: "RSA key that signs workload tokens (created if missing); keep it outside /tmp"},
	{Env: "ASP_OIDC_KEY_PREV", Help: "the previous RSA key, published in the JWKS during a rotation"},
	{Env: "ASP_OIDC_ISSUER", Default: "http://<listen address>", Help: "OIDC issuer of the workload tokens"},
	{Env: "ASP_ATTEST_KEY", Default: "$TMPDIR/asp-attest-key.pem", Help: "ECDSA key of the software attestor (created if missing); keep it outside /tmp"},
	{Env: "ASP_ATTEST_PUB", Help: "public key that replaces ASP_ATTEST_KEY's for verifying evidence"},
	{Env: "ASP_ATTEST_TRUSTED_PUBS", Help: "PEM bundle of more public keys or certificates trusted for evidence"},
	{Env: "ASP_ATTEST_MAX_AGE", Default: "10m", Help: "how old evidence may be for the token claim"},
	{Env: "ASP_ATTEST_ALLOWED_IMAGES", Help: "JSON file of the kernel/image digests a node may attest (node-agent --print-measurement)"},
	{Env: "ASP_FENCE_PROVIDER", Default: "noop", Help: "how a lost node is powered off: noop, redfish or ipmi"},
	{Env: "ASP_FENCE_USER", Help: "BMC user of the fence provider"},
	{Env: "ASP_FENCE_PASS", Help: "BMC password of the fence provider", Secret: true},

	// Egress, idle and retention.
	{Env: "ASP_EGRESS_DEFAULT_ALLOW", Default: "1 with the memory store, 0 with Postgres", Help: "what a tenant with no egress rules may reach: 1 everything, 0 nothing"},
	{Env: "ASP_EGRESS_DENY_DEFAULT", Help: "the old name of ASP_EGRESS_DEFAULT_ALLOW, with the opposite meaning"},
	{Env: "ASP_SANDBOX_IDLE_TIMEOUT", Default: "off", Help: "stop a sandbox idle this long (2h recommended; 0 or off disables)"},
	{Env: "ASP_SANDBOX_IDLE_SWEEP", Default: "1m", Help: "how often the idle reaper runs"},
	{Env: "ASP_STOPPED_SANDBOX_TTL", Default: "7d", Help: "a stopped sandbox is deleted with its disk after this (0 or off keeps them)"},
	{Env: "ASP_MAX_STOPPED_PER_TENANT", Help: "the oldest stopped sandboxes of a tenant beyond this are deleted"},
	{Env: "ASP_RETENTION_SWEEP", Default: "1m", Help: "how often the retention sweep runs"},
}
