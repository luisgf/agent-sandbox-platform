-- ADR-0020: the groups the owner's IdP token carried when it created the sandbox (the values of
-- the role claim, as they came). Its workload tokens carry them as `groups`. Empty for a sandbox
-- without an owner, or from before this migration: those tokens have no such claim.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS owner_groups text[] NOT NULL DEFAULT '{}'::text[];
