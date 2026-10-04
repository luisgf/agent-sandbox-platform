## What and why

<!-- One or two sentences. Link the issue or ADR if there is one. -->

## How it was tested

- [ ] `make test`
- [ ] Smoke scripts (`make smoke`, `make smoke-asp`) if a control-plane, node-agent or CLI flow changed
- [ ] On a real KVM / Cloud Hypervisor host (if not, add the `needs-kvm` label)

## Checklist

- [ ] Title follows Conventional Commits (`feat(node-agent): …`, `fix(egress): …`, `docs: …`)
- [ ] README or `docs/ops-*.md` updated if behaviour or flags changed
- [ ] A new or changed trust boundary is explained in an ADR under `docs/adr/`
- [ ] No secrets, keys or private lab details in the diff
