# Contributing

Thanks for looking at ASP. This is how a change gets in. For what the project is, start with the [README](README.md) and [the architecture](docs/architecture.md); [the glossary](docs/reference/glossary.md) explains the words, and the [ADRs](docs/adr/README.md) say why things are the way they are.

Security problems are not reported as issues or pull requests: see [SECURITY.md](SECURITY.md).

## Before you start

- **Look for an issue first, and open one for anything that is not small.** A bug, a missing feature, a decision that needs discussing. A pull request that arrives without one is welcome when it is a fix you can explain in two sentences.
- **A new trust boundary, a new way of doing something that already has one, or a decision that costs something to undo** is an [ADR](docs/adr/README.md): write it (Spanish, like the others) before the code, or with it.
- **What is already decided** is in the ADRs and in the [roadmap](docs/roadmap.md), under *Fuera de alcance*. A change that goes against one of them starts by saying why, in an issue.

## Layout

```text
cli/            asp, the command line (and asp-server)       Go
control-plane/  API, scheduler, node monitor, stores, PKI    Go, with SQL migrations for Postgres and SQLite
node-agent/     runs the VMs of a node                       Go
pod-daemon/     the daemon inside the guest                  Rust
images/guest/   the guest image and its helpers
scripts/        smokes, installer, docs checks
docs/           the documentation (Spanish today; one language is #142)
```

The Go modules are separate (`go.mod` in each): run `go` commands inside the module you are changing. The code, its comments, the error messages and the commit messages are English; the documents under `docs/` are Spanish for now, and `README.md`, `CHANGELOG.md`, `SECURITY.md` and this file are English.

## Build and test

You need the Go version each module's `go.mod` asks for, a Rust toolchain for `pod-daemon`, and Python 3 for the documentation checks. `shellcheck` and `sqlite3` are used when they are installed.

```sh
make test          # Go (with -race for the control plane and the node agent), Rust, and the guest helper
make lint          # gofmt, go vet and staticcheck (pinned in the Makefile) in every module, and the shell scripts
make docs          # rewrites the generated pages under docs/reference (see below)
make check-docs    # the links of every Markdown file, and the status of every ADR
make smoke         # the dry-run end-to-end scripts: enrollment and exec, identity and egress, reconcile, two nodes
make smoke-asp     # the CLI against a control plane in dry-run
```

- **No KVM is needed for any of that.** The node agent runs with `--dry-run` (`FakeVMM`) in the tests and the smokes: they exercise the control plane, the scheduler, the reconciler and the protocol, but isolate nothing. What needs a KVM host (real VMs, `enforce` for nftables, `virtiofs`, `--local-net`) has its own scripts (`make e2e-kvm`, `smoke-egress-kvm`, `smoke-vmm-user-kvm`; [how to run them](docs/how-to/e2e-kvm.md)), and CI does not run them. **Say in the pull request whether you ran them**; if you did not, the change gets the `needs-kvm` label.
- **The stores.** The control plane has three: memory, SQLite and Postgres, and they are one contract. The API tests run on each (`ASP_TEST_STORE=memory|sqlite|postgres` keeps one pass), and Postgres joins in when `DATABASE_URL` is set: `docker compose up -d postgres`, then `export DATABASE_URL=postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable`. A change to a store method needs the same change in the other two, a step in the parity script (`internal/store`), and a migration in both `control-plane/migrations/` and `migrations/sqlite/`.
- **On macOS** CI runs on Ubuntu, so some things differ: set a short `TMPDIR` (`export TMPDIR=/tmp/aspgo`; tests bind unix sockets and macOS caps their paths at 104 bytes, so keep test names short too), `unset SSH_AUTH_SOCK` for the node-agent's SSH-agent tests, and expect `TestLocalNetUpAppliesMockWireGuardAndDownDeletesIt` to need `ASP_LOCAL_NET_OS=linux`.

## What a change must carry

- **Tests.** A bug fix has a test that fails without it. Behaviour that touches concurrency goes through `-race`.
- **The generated pages.** Several pages are written from the code, and a test fails when one is out of date: the settings tables and `asp <command> -h` ([`docs/reference`](docs/reference/configuration.md)), and the API reference, which comes from [`control-plane/internal/api/openapi.yaml`](control-plane/internal/api/openapi.yaml). Add a setting with its description and its group, add a route or a field to the OpenAPI document (tests check it against the routes, the Go types and what the handlers answer, and the CLI client against it), then run `make docs` and commit what it changes.
- **Docs and the changelog.** Update the page that explains the thing you changed. A change a user can see (a feature, a changed or removed setting, a behaviour that moves) goes in [CHANGELOG.md](CHANGELOG.md) under *Unreleased*, and one that breaks something says what to do under *Changed* or *Removed*.
- **No secrets** in the diff: keys, tokens, passwords, or details of a private host.

## Style

- `gofmt`, `go vet` and the pinned `staticcheck` must be clean (`make lint`). Comments say **why**, not what; follow the surrounding code in density and naming.
- Error messages tell the person what to do, and a setting's help text is what the operator reads in the generated reference: write it for them.
- Keep a change to one thing. A refactor and a fix are two pull requests.

## Commits and pull requests

- Branch from `main`. Titles are [Conventional Commits](https://www.conventionalcommits.org/): `feat(node-agent): …`, `fix(egress): …`, `docs: …`, `test: …`, `ci: …`. The body says what changed and why, and the issue it closes (`Closes #123`).
- Fill in the [pull request template](.github/pull_request_template.md): what and why, how it was tested, and its checklist.
- **CI must be green** before a merge. It runs `gofmt`, `go vet`, the tests and `staticcheck` in the four Go modules, the control-plane store tests on Postgres, the two-node dry-run smoke, and the Rust tests of the pod-daemon (and a build check for `aarch64`). Pull requests are merged with a merge commit.
- A pull request with merge conflicts gets no CI: merge `main` into your branch first.

## Releases

A release is a tag; how one is cut, and what it contains, is in [publicar una versión](docs/how-to/release.md). The changelog is written by hand as you go, so it is ready when the tag is.

## License

ASP is under the [Apache License 2.0](LICENSE), and your contributions are licensed under it (section 5 of the license).
