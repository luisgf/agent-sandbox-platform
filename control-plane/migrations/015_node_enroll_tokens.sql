-- Single-use node enrollment tokens issued by an admin (POST /v1/nodes/enroll-tokens).
-- Only the SHA-256 of a token is stored. A token pinned to a node id is the only
-- way to re-enroll a node that holds a live certificate; the shared bootstrap
-- token may only enroll a node id without a certificate or a revoked one.
CREATE TABLE IF NOT EXISTS node_enroll_tokens (
    hash       text PRIMARY KEY,
    node_id    text,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    used_by    text NOT NULL DEFAULT '',
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS node_enroll_tokens_expires_idx
    ON node_enroll_tokens (expires_at);
