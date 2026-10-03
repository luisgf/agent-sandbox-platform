-- Idle auto-stop: last successful activity and why a sandbox was stopped.
-- Existing rows get last_activity_at = now() via the column default, so enabling
-- the reaper does not immediately reap every sandbox created hours ago.
-- The idle clock for those rows starts at migration time; exec/start refresh it.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS last_activity_at timestamptz NOT NULL DEFAULT now();

ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS stop_reason text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sandboxes_idle_active_idx
    ON sandboxes (last_activity_at)
    WHERE state IN ('requested', 'scheduled', 'starting', 'running', 'paused');
