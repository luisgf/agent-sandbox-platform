-- The SQLite schema, as the Postgres migrations 001 to 024 leave it. It is one file because
-- there is no SQLite database from before it to carry forward; from 025 on, a migration has a
-- twin here with the same number (a test fails when the two directories disagree).
--
-- Differences from Postgres, all of them in how a value is stored:
--   * timestamps are TEXT, always "2006-01-02T15:04:05.000000Z" (UTC, microseconds): that
--     fixed width is what makes the text compare in time order. The store writes every
--     one, so no column has a clock default.
--   * booleans are INTEGER 0 or 1; arrays and JSON are TEXT holding JSON.
--   * ids are made by the store (a UUID), not by the database.

CREATE TABLE tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE nodes (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    endpoint            TEXT NOT NULL DEFAULT '',
    agent_endpoint      TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT 'unknown' CHECK (state IN ('unknown', 'ready', 'draining', 'offline')),
    vmm_profiles        TEXT NOT NULL DEFAULT '["cloud-hypervisor"]',
    capacity_cpu        INTEGER NOT NULL DEFAULT 0 CHECK (capacity_cpu >= 0),
    capacity_mem_mib    INTEGER NOT NULL DEFAULT 0 CHECK (capacity_mem_mib >= 0),
    max_sandboxes       INTEGER NOT NULL DEFAULT 0 CHECK (max_sandboxes >= 0),
    cordoned            INTEGER NOT NULL DEFAULT 0 CHECK (cordoned IN (0, 1)),
    accepts_work        INTEGER NOT NULL DEFAULT 1 CHECK (accepts_work IN (0, 1)),
    local_net_dial      TEXT NOT NULL DEFAULT '',
    agent_instance_id   TEXT NOT NULL DEFAULT '',
    agent_version       TEXT NOT NULL DEFAULT '',
    guest_kernel_digest TEXT NOT NULL DEFAULT '',
    guest_image_digest  TEXT NOT NULL DEFAULT '',
    egress_enforced     INTEGER NOT NULL DEFAULT 0 CHECK (egress_enforced IN (0, 1)),
    cert_fingerprint    TEXT NOT NULL DEFAULT '',
    cert_serial         TEXT NOT NULL DEFAULT '',
    cert_not_after      TEXT,
    fence_token         TEXT NOT NULL DEFAULT '',
    fence_endpoint      TEXT NOT NULL DEFAULT '',
    enrolled_at         TEXT,
    revoked_at          TEXT,
    last_seen_at        TEXT,
    disk_free_mib       INTEGER,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);
CREATE INDEX nodes_cert_fingerprint_idx ON nodes (cert_fingerprint) WHERE cert_fingerprint <> '';

CREATE TABLE sandboxes (
    id                         TEXT PRIMARY KEY,
    tenant_id                  TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    node_id                    TEXT REFERENCES nodes(id) ON DELETE SET NULL,
    state                      TEXT NOT NULL DEFAULT 'requested' CHECK (
        state IN ('requested', 'scheduled', 'starting', 'running', 'paused', 'stopping', 'stopped', 'failed', 'deleting', 'deleted')
    ),
    vmm_profile                TEXT NOT NULL DEFAULT 'cloud-hypervisor',
    image_ref                  TEXT NOT NULL,
    cpu_millis                 INTEGER NOT NULL CHECK (cpu_millis > 0),
    memory_mib                 INTEGER NOT NULL CHECK (memory_mib > 0),
    state_version              INTEGER NOT NULL DEFAULT 1 CHECK (state_version > 0),
    owner_sub                  TEXT NOT NULL DEFAULT '',
    owner_email                TEXT NOT NULL DEFAULT '',
    last_activity_at           TEXT NOT NULL,
    stop_reason                TEXT NOT NULL DEFAULT '',
    status_detail              TEXT NOT NULL DEFAULT '',
    boot_count                 INTEGER NOT NULL DEFAULT 1,
    booted_at                  TEXT,
    stopped_at                 TEXT,
    workspace_host_path        TEXT NOT NULL DEFAULT '',
    local_net                  INTEGER NOT NULL DEFAULT 0 CHECK (local_net IN (0, 1)),
    local_net_state            TEXT NOT NULL DEFAULT 'off',
    local_net_attached_at      TEXT,
    local_net_grant_expires_at TEXT,
    local_net_grant_hash       TEXT NOT NULL DEFAULT '',
    local_net_client_public    TEXT NOT NULL DEFAULT '',
    local_net_node_public      TEXT NOT NULL DEFAULT '',
    local_net_listen_port      INTEGER NOT NULL DEFAULT 0,
    local_net_node_addr        TEXT NOT NULL DEFAULT '',
    local_net_client_addr      TEXT NOT NULL DEFAULT '',
    node_lease_until           TEXT,
    created_at                 TEXT NOT NULL,
    updated_at                 TEXT NOT NULL
);
CREATE INDEX sandboxes_tenant_created_idx ON sandboxes (tenant_id, created_at DESC);
CREATE INDEX sandboxes_tenant_owner_idx ON sandboxes (tenant_id, owner_sub);
CREATE INDEX sandboxes_node_state_idx ON sandboxes (node_id, state) WHERE node_id IS NOT NULL;
CREATE INDEX sandboxes_idle_active_idx ON sandboxes (last_activity_at)
    WHERE state IN ('requested', 'scheduled', 'starting', 'running', 'paused');
CREATE INDEX sandboxes_stopped_idx ON sandboxes (stopped_at) WHERE state = 'stopped';

CREATE TABLE sandbox_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    sandbox_id TEXT NOT NULL REFERENCES sandboxes(id) ON DELETE CASCADE,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    event_type TEXT NOT NULL,
    from_state TEXT,
    to_state   TEXT,
    actor      TEXT NOT NULL,
    actor_sub  TEXT NOT NULL DEFAULT '',
    request_id TEXT,
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX sandbox_events_sandbox_idx ON sandbox_events (sandbox_id, id);
CREATE INDEX sandbox_events_tenant_created_idx ON sandbox_events (tenant_id, created_at DESC);
CREATE INDEX sandbox_events_actor_sub_idx ON sandbox_events (actor_sub) WHERE actor_sub <> '';

CREATE TABLE node_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id    TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    actor      TEXT NOT NULL DEFAULT 'system',
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX node_events_node_idx ON node_events (node_id, id);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT 'tenant' CHECK (scope IN ('tenant', 'platform')),
    key_prefix   TEXT NOT NULL,
    secret_hash  TEXT NOT NULL,
    last_used_at TEXT,
    expires_at   TEXT,
    revoked_at   TEXT,
    created_at   TEXT NOT NULL,
    UNIQUE (tenant_id, name),
    UNIQUE (key_prefix)
);
CREATE UNIQUE INDEX api_keys_secret_hash_key ON api_keys (secret_hash);

CREATE TABLE tenant_egress_rules (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    host_pattern TEXT NOT NULL,
    port         INTEGER CHECK (port IS NULL OR (port > 0 AND port <= 65535)),
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE (tenant_id, host_pattern, port)
);
CREATE INDEX tenant_egress_rules_tenant_idx ON tenant_egress_rules (tenant_id) WHERE enabled = 1;

CREATE TABLE sandbox_attestations (
    sandbox_id   TEXT PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    image_digest TEXT NOT NULL DEFAULT '',
    vmm_profile  TEXT NOT NULL DEFAULT '',
    cid          INTEGER NOT NULL DEFAULT 0,
    statement_ts TEXT NOT NULL,
    alg          TEXT NOT NULL DEFAULT 'ES256',
    key_id       TEXT NOT NULL DEFAULT '',
    signature    TEXT NOT NULL,
    bundle       TEXT NOT NULL DEFAULT '{}',
    received_at  TEXT NOT NULL
);
CREATE INDEX sandbox_attestations_received_idx ON sandbox_attestations (received_at DESC);

CREATE TABLE node_cert_revocations (
    fingerprint TEXT PRIMARY KEY,
    node_id     TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    serial      TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    revoked_at  TEXT NOT NULL
);
CREATE INDEX node_cert_revocations_node_idx ON node_cert_revocations (node_id);

CREATE TABLE node_enroll_tokens (
    hash       TEXT PRIMARY KEY,
    node_id    TEXT,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    used_by    TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);
CREATE INDEX node_enroll_tokens_expires_idx ON node_enroll_tokens (expires_at);
