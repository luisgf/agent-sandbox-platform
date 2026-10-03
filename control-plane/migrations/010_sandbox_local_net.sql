-- Full-tunnel local net (ADR-0010). Default off. No CIDR policy column:
-- v1 is the whole default route or nothing. The grant hash is not the grant.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS local_net boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS local_net_state text NOT NULL DEFAULT 'off',
    ADD COLUMN IF NOT EXISTS local_net_attached_at timestamptz,
    ADD COLUMN IF NOT EXISTS local_net_grant_expires_at timestamptz,
    ADD COLUMN IF NOT EXISTS local_net_grant_hash text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS local_net_client_public text NOT NULL DEFAULT '';
