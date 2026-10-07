-- Free space of the node's --disk-dir, as the node reported it on its last
-- heartbeat (ADR-0012). NULL until a node that reports it has heartbeated.
-- Sandbox disks that a stop keeps live there, so it is what runs out.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS disk_free_mib bigint;
