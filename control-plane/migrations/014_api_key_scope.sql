-- Tenant isolation: an API key acts within its tenant unless it is platform-scoped
-- (operations tooling). The control plane makes ASP_BOOTSTRAP_API_KEY platform-scoped
-- at start-up; any other key defaults to its tenant.
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'tenant' CHECK (scope IN ('tenant', 'platform'));
