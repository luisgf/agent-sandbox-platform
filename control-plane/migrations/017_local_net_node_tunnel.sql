-- Local-net tunnel parameters the node allocates and publishes with its public
-- key (ADR-0010): the UDP port of the node WireGuard device and both ends of
-- the tunnel /30. They used to be hashed from the 8-character short id, which
-- collides; the grant now hands out what the node chose.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS local_net_listen_port integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS local_net_node_addr text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS local_net_client_addr text NOT NULL DEFAULT '';
