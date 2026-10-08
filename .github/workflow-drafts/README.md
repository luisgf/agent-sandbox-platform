# Workflow drafts

Workflows that are written and tested but not enabled. Moving a file out of here to
`.github/workflows/` enables it (the token that pushes it needs the `workflow` scope:
`gh auth refresh -s workflow`):

```sh
git mv .github/workflow-drafts/nightly-kvm.yml .github/workflows/nightly-kvm.yml
git commit -m "ci: enable the nightly KVM lane" && git push
```

Each draft says at its top what it needs (secrets, runners, settings).

| Draft | What it does | Needs |
|---|---|---|
| `guest-image.yml` | Builds the guest image twice and fails if the two `SHA256SUMS` differ | nothing else |
| `guest-kernel.yml` | Builds the guest kernel twice (it compiles Linux) and fails if the two differ | nothing else |
| `nightly-kvm.yml` | The real end to end on a KVM host, nightly and for a pull request labelled `needs-kvm` | a self-hosted runner with KVM |
