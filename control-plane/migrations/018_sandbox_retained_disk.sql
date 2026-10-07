-- ADR-0012: stopping a sandbox keeps its disk, deleting removes it.
--
-- deleting: the node is removing the VM and the disk. deleted: final; the row
-- stays, because sandbox_events cascades from it and the audit trail must
-- outlive the sandbox. stopped stops being final: a resume moves it back to
-- requested on the same node.
ALTER TABLE sandboxes DROP CONSTRAINT IF EXISTS sandboxes_state_check;

ALTER TABLE sandboxes
    ADD CONSTRAINT sandboxes_state_check CHECK (
        state IN ('requested', 'scheduled', 'starting', 'running', 'paused', 'stopping', 'stopped', 'failed', 'deleting', 'deleted')
    );

-- Why a node last reported failed, or a resume went back to stopped.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS status_detail text NOT NULL DEFAULT '';

-- Starts so far: 1 at the first boot, one more per resume. Above 1 the node
-- boots the disk a stop kept instead of cloning a new one.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS boot_count integer NOT NULL DEFAULT 1;

-- When the node reported the sandbox stopped; the retention TTL counts from it.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS stopped_at timestamptz;

UPDATE sandboxes SET stopped_at = updated_at WHERE state = 'stopped' AND stopped_at IS NULL;

CREATE INDEX IF NOT EXISTS sandboxes_stopped_idx
    ON sandboxes (stopped_at)
    WHERE state = 'stopped';
