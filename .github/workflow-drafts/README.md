# Workflow drafts

Workflows that are written and tested but not enabled, because the token they were pushed
with cannot create or change files under `.github/workflows/` (GitHub asks for the
`workflow` scope: `gh auth refresh -s workflow`). To enable one:

```sh
git mv .github/workflow-drafts/release.yml .github/workflows/release.yml
git commit -m "ci: enable the release workflow" && git push
```

Each draft says at its top what it needs (secrets, runners, settings).
