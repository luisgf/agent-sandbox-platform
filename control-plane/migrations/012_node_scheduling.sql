-- Multi-node placement (ADR-0011). capacity_cpu (cores) and capacity_mem_mib exist since 001.
-- 0 in a capacity column means "not enforced" for that dimension.
-- cordoned is set by an admin (no new placements); accepts_work is the agent's --reconcile;
-- local_net_dial is the host:port a laptop dials for this node's local-net tunnels.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS max_sandboxes integer NOT NULL DEFAULT 0 CHECK (max_sandboxes >= 0);
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS cordoned boolean NOT NULL DEFAULT false;
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS accepts_work boolean NOT NULL DEFAULT true;
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS local_net_dial text NOT NULL DEFAULT '';
