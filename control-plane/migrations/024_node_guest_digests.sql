-- #131: the digests of the guest kernel and base image a node boots sandboxes from, as it said
-- on its last register ("" until it has hashed them, and for a node that predates the fields),
-- so asp node list shows which image each node runs.
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS guest_kernel_digest text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS guest_image_digest text NOT NULL DEFAULT '';
