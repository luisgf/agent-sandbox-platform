-- ADR-0007 phase 1: owner_sub / owner_email on sandboxes + actor_sub on sandbox_events.
-- Lab-compatible: empty owner_sub / actor_sub allowed until IdP JWT (phase 2).
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS owner_sub text NOT NULL DEFAULT '';
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS owner_email text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sandboxes_tenant_owner_idx
    ON sandboxes (tenant_id, owner_sub);

ALTER TABLE sandbox_events
    ADD COLUMN IF NOT EXISTS actor_sub text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS sandbox_events_actor_sub_idx
    ON sandbox_events (actor_sub)
    WHERE actor_sub <> '';
