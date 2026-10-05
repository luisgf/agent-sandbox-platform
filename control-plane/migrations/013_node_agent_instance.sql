-- Each node-agent process sends a random id when it registers (ADR-0011). A new id
-- means the agent restarted and lost track of its running VMs: their sandboxes fail.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS agent_instance_id text NOT NULL DEFAULT '';
