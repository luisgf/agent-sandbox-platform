-- #127: the build of the node-agent, as it said on its last register ("" for an
-- agent that predates the field), so a rollout can be followed with asp node list.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS agent_version text NOT NULL DEFAULT '';
