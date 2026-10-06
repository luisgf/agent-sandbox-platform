-- When the node's current certificate expires: operators see it in GET /v1/nodes
-- and asp node list, and node agents renew a third of the lifetime ahead.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS cert_not_after timestamptz;
