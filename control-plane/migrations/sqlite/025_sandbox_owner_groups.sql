-- ADR-0020: the groups the owner's IdP token carried when it created the sandbox (the values of
-- the role claim, as they came), as a JSON array. Its workload tokens carry them as `groups`.
-- '[]' for a sandbox without an owner, or from before this migration: those tokens have no such claim.
ALTER TABLE sandboxes ADD COLUMN owner_groups TEXT NOT NULL DEFAULT '[]';
