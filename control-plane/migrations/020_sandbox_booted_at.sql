-- #112: when the node first reported the sandbox running.
--
-- The node decided whether to reuse a disk or clone a new one from boot_count
-- above 1. A sandbox stopped before it ever ran (stopped while requested, by hand
-- or by the idle reaper) and then resumed has boot_count 2 and no disk, and its
-- node reported failed disk_lost. booted_at says whether a first boot happened.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS booted_at timestamptz;

-- Rows from before: booted when an event says they ran, or they are running or
-- paused now, or they were resumed (boot_count above 1: a stop kept a disk).
UPDATE sandboxes s SET booted_at = COALESCE(
    (SELECT min(e.created_at) FROM sandbox_events e WHERE e.sandbox_id = s.id AND e.to_state = 'running'),
    s.updated_at)
WHERE s.booted_at IS NULL
  AND (s.state IN ('running', 'paused')
       OR s.boot_count > 1
       OR EXISTS (SELECT 1 FROM sandbox_events e WHERE e.sandbox_id = s.id AND e.to_state = 'running'));
