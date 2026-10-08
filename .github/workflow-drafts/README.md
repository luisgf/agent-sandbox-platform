# Workflow drafts

Workflows that are written and tested but not enabled, because the token they were pushed
with cannot create or change files under `.github/workflows/` (GitHub asks for the
`workflow` scope: `gh auth refresh -s workflow`). To enable one:

```sh
git mv .github/workflow-drafts/release.yml .github/workflows/release.yml
git commit -m "ci: enable the release workflow" && git push
```

Each draft says at its top what it needs (secrets, runners, settings).

| Draft | What it does | Needs |
|---|---|---|
| `release.yml` | Cuts a release when a `vX.Y.Z` tag is pushed ([publish a version](../../docs/how-to/release.md)) | only the repository's `GITHUB_TOKEN`; make the `asp-control-plane` package on ghcr.io public once |
| `guest-image.yml` | Builds the guest image twice and fails if the two `SHA256SUMS` differ | nothing else |
| `guest-kernel.yml` | Builds the guest kernel twice (it compiles Linux) and fails if the two differ | nothing else |
| `nightly-kvm.yml` | The real end to end on a KVM host, nightly and for a pull request labelled `needs-kvm` | a self-hosted runner with KVM |
| `docs.yml` | The documentation checks (`make check-docs`): links, reachability from `docs/README.md`, ADR statuses | nothing else |
