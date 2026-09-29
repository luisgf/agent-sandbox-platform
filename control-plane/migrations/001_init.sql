CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE tenants (
    id text PRIMARY KEY,
    name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE nodes (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    endpoint text NOT NULL DEFAULT '',
    state text NOT NULL DEFAULT 'unknown' CHECK (state IN ('unknown', 'ready', 'draining', 'offline')),
    vmm_profiles text[] NOT NULL DEFAULT ARRAY['cloud-hypervisor']::text[],
    capacity_cpu integer NOT NULL DEFAULT 0 CHECK (capacity_cpu >= 0),
    capacity_mem_mib integer NOT NULL DEFAULT 0 CHECK (capacity_mem_mib >= 0),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sandboxes (
    id text PRIMARY KEY DEFAULT (gen_random_uuid()::text),
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    node_id text REFERENCES nodes(id) ON DELETE SET NULL,
    state text NOT NULL DEFAULT 'requested' CHECK (
        state IN ('requested', 'scheduled', 'starting', 'running', 'paused', 'stopping', 'stopped', 'failed')
    ),
    vmm_profile text NOT NULL DEFAULT 'cloud-hypervisor',
    image_ref text NOT NULL,
    cpu_millis integer NOT NULL CHECK (cpu_millis > 0),
    memory_mib integer NOT NULL CHECK (memory_mib > 0),
    state_version bigint NOT NULL DEFAULT 1 CHECK (state_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sandboxes_tenant_created_idx ON sandboxes (tenant_id, created_at DESC);
CREATE INDEX sandboxes_node_state_idx ON sandboxes (node_id, state) WHERE node_id IS NOT NULL;

CREATE TABLE sandbox_events (
    id bigserial PRIMARY KEY,
    sandbox_id text NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    event_type text NOT NULL,
    from_state text,
    to_state text,
    actor text NOT NULL,
    request_id text,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sandbox_events_sandbox_idx ON sandbox_events (sandbox_id, id);
CREATE INDEX sandbox_events_tenant_created_idx ON sandbox_events (tenant_id, created_at DESC);

CREATE TABLE node_events (
    id bigserial PRIMARY KEY,
    node_id text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    event_type text NOT NULL,
    actor text NOT NULL DEFAULT 'system',
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX node_events_node_idx ON node_events (node_id, id);

CREATE TABLE api_keys (
    id text PRIMARY KEY DEFAULT (gen_random_uuid()::text),
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name text NOT NULL,
    key_prefix text NOT NULL,
    secret_hash text NOT NULL,
    last_used_at timestamptz,
    expires_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name),
    UNIQUE (key_prefix)
);
