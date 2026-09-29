-- Fase 2d: node client-cert serial + revocation so stolen/expired certs cannot keep talking to CP.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS cert_serial text NOT NULL DEFAULT '';
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS revoked_at timestamptz;

CREATE TABLE IF NOT EXISTS node_cert_revocations (
    fingerprint text PRIMARY KEY,
    node_id     text NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    serial      text NOT NULL DEFAULT '',
    reason      text NOT NULL DEFAULT '',
    revoked_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS node_cert_revocations_node_idx
    ON node_cert_revocations (node_id);
