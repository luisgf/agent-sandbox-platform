-- Tenant egress allowlist rules (deny-by-default when ≥1 enabled rule).
CREATE TABLE IF NOT EXISTS tenant_egress_rules (
    id text PRIMARY KEY DEFAULT (gen_random_uuid()::text),
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    host_pattern text NOT NULL,
    port integer CHECK (port IS NULL OR (port > 0 AND port <= 65535)),
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, host_pattern, port)
);
CREATE INDEX IF NOT EXISTS tenant_egress_rules_tenant_idx
    ON tenant_egress_rules (tenant_id) WHERE enabled;
