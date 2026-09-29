-- Multi-node soft fencing via sandbox leases (no STONITH).
-- node_lease_until: claim/renew extends; expired starting|running may be reclaimed.
-- fence_token: optional opaque token on the node (ops/docs; not hardware fence).

ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS node_lease_until timestamptz;

ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS fence_token text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sandboxes_lease_idx
    ON sandboxes (node_lease_until)
    WHERE node_lease_until IS NOT NULL;
