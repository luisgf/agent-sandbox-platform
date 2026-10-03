-- Node WireGuard public key for the per-sandbox device. Not a secret.
-- The private key stays on the node (mode 0600) and is never stored here.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS local_net_node_public text NOT NULL DEFAULT '';
