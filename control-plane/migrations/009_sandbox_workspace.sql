-- Host directory the session asked to share into the guest (virtiofs spec).
-- Empty string means no share. Storing the path does not mount it: the
-- node-agent records it for FakeVMM / dry-run. Cloud Hypervisor does not
-- start virtiofsd, so KVM guests do not see this directory.
ALTER TABLE sandboxes
    ADD COLUMN IF NOT EXISTS workspace_host_path text NOT NULL DEFAULT '';
