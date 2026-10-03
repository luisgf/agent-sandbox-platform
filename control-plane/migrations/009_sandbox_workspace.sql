-- Host directory the session asked to share into the guest.
-- Empty string means no share. A non-empty path is the virtiofs source:
-- the node-agent starts virtiofsd and Cloud Hypervisor gets fs tag
-- "workspace". The guest still has to mount that tag on /workspace.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS workspace_host_path text NOT NULL DEFAULT '';
