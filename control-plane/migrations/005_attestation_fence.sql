-- Remote attestation evidence + fence endpoint metadata for STONITH providers.

ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS fence_endpoint text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS sandbox_attestations (
    sandbox_id     text PRIMARY KEY REFERENCES sandboxes(id) ON DELETE CASCADE,
    node_id        text NOT NULL,
    image_digest   text NOT NULL DEFAULT '',
    vmm_profile    text NOT NULL DEFAULT '',
    cid            integer NOT NULL DEFAULT 0,
    statement_ts   timestamptz NOT NULL,
    alg            text NOT NULL DEFAULT 'ES256',
    key_id         text NOT NULL DEFAULT '',
    signature      text NOT NULL,
    bundle         jsonb NOT NULL DEFAULT '{}',
    received_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sandbox_attestations_received_idx
    ON sandbox_attestations (received_at DESC);
