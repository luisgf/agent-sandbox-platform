# Changelog

All notable changes to ASP are written here, newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the versions follow
[Semantic Versioning](https://semver.org/): while the major version is 0, a minor
release may change behaviour or settings, and says so under **Changed** or **Removed**.

How a release is cut: [docs/how-to/release.md](docs/how-to/release.md).

## [Unreleased]

The first release will be 0.1.0. Until then this is what `main` has.

### Added

- **Sandboxes that keep their state.** Stopping a sandbox keeps its disk; `asp session resume`
  boots it again on that disk, and `asp session rm` deletes it. Stopped sandboxes expire after
  7 days (`ASP_STOPPED_SANDBOX_TTL`) or beyond a per-tenant cap. [ADR-0012](docs/adr/0012-retained-disks.md)
- **Several nodes.** A scheduler places each sandbox on a node with room (`spread` or `binpack`,
  CPU overcommit, memory never overcommitted), refuses with a 503 and the reasons when none fits,
  and detects lost nodes, fences them and fails their sandboxes. `asp node list|cordon|uncordon`.
  The control plane and the nodes talk over mutual TLS. [ADR-0011](docs/adr/0011-multi-node.md)
- **VMs outlive the node-agent.** Each microVM and its virtiofsd run in a systemd unit of their
  own; a restarted agent adopts the VMs it finds. [ADR-0014](docs/adr/0014-vms-outlive-the-agent.md)
- **Cloud Hypervisor as an unprivileged user,** one per VM, without capabilities, in a hardened
  unit. [ADR-0015](docs/adr/0015-unprivileged-vmm.md)
- **`asp doctor` and `asp node doctor`:** the node checks KVM, the hypervisor, images, disk space,
  nftables and the clock, and says how to fix what it finds.
- **API keys** with tenant or platform scope, managed through the API and `asp apikey`.
- **Metrics and diagnostics:** Prometheus metrics and pprof on a loopback listener, a build info
  gauge, `--version` on every binary, the node-agent version in `asp node list`.
- **Guest:** hostname, resolver, proxy and a clock that follows the host; the command runs as the
  workspace owner unless the host asks for root.
- **Packaging:** `make build` builds every binary; goreleaser builds archives, `.deb`/`.rpm`
  packages with their systemd units, SBOMs and a control plane container image
  ([.goreleaser.yaml](.goreleaser.yaml)).
- **A guest image that is the same bytes in every build** (pinned bases, a Debian snapshot, locked
  crates and modules, a fixed ext4 layout): `scripts/build-guest-image.sh [--verify]` makes
  `rootfs.img`, `SHA256SUMS` and an `image.json` with the package list. `asp image pull` installs a
  release's kernel and image and refuses anything that does not match its checksums; the node-agent
  refuses to boot from a file its `SHA256SUMS` lists with another digest (`--guest-verify`), and
  `asp node list` shows the digest of the image each node runs.
- **An installer**: `curl -fsSL …/releases/latest/download/install.sh | sudo sh` (roles `cli`, `server`,
  `agent`) installs a release's packages after checking them against `SHA256SUMS`, writes the
  settings of the `INSTALL_ASP_*` variables into a drop-in under `/etc/asp`, pulls the guest image and
  starts the service; `asp-killall.sh` and `asp-uninstall.sh` take ASP away again.
- **Joining a node with a private certificate:** the CLI trusts a PEM file named by `ASP_CA_FILE`, reads its key from `ASP_API_KEY_FILE`, and `asp node enroll-token` prints the installer command to add the node, with the
  fingerprint of the certificate (`INSTALL_ASP_CA_SHA256`): the installer reads the certificate from the server, refuses it if the fingerprint differs, and trusts it.
- **A configuration file** for each component, with drop-ins like k3s's: `/etc/asp/agent.yaml` and
  `agent.yaml.d/*.yaml` (node-agent), `server.yaml` (control plane), `asp.yaml` (CLI, also
  `~/.config/asp/asp.yaml`). The order is flag, environment, file, default, in all three; an unknown
  key stops the start and names the nearest setting. `--print-config` (node-agent, control plane) and
  `asp config show [--effective]` list every setting with its value and where it came from, with the
  credentials hidden. [docs/how-to/config-file.md](docs/how-to/config-file.md)
- **`scripts/e2e-kvm.sh`**: the life of a sandbox on a real KVM host (a workspace, a command as its owner
  and as root, data nobody syncs, stop, a clean disk, resume, delete, a clean node), on a throwaway stack or
  against a deployment through its API; the nightly workflow runs it with the other KVM smokes.
- A Postgres run of the API tests, and a parity script that holds the memory and Postgres stores to
  one contract.
- **ASP on one host** ([docs/how-to/single-host.md](docs/how-to/single-host.md), [ADR-0016](docs/adr/0016-single-host.md)):
  `INSTALL_ASP_ROLE=standalone` installs four packages and starts `asp-server`, which makes the database,
  the keys, a self-signed TLS certificate, an administration key and the node token, runs a control plane
  (without privileges) and a node, stops them in order, and leaves the `asp` command of the host
  configured. Three commands to the first session. `--profile lab` runs a node without VMs: no KVM, no
  root. `scripts/smoke-standalone.sh` covers it with the real binaries in CI.
- **A SQLite store** for a single host: `ASP_DATABASE_URL=sqlite:///var/lib/asp/server/asp.db` keeps
  the state in one private file (no CGO, no database server). The same parity script, shared store
  tests and API suite run on it, with no server needed, so every test run covers it.
- **A reference generated from the code.** [`docs/reference/`](docs/reference/configuration.md) lists every
  setting of the control plane, the node-agent, `asp` and `asp-server`, and every `asp` command with its
  flags. `make docs` writes it and a test in each module fails when a page is out of date, so the hand-written
  lists of the READMEs and of `bare-metal-ch.md` are gone. `scripts/check-doc-links.py` checks the relative
  links of the Markdown files.
- **Pages that were missing.** A [glossary](docs/reference/glossary.md); one page of [known limits](docs/reference/limitations.md)
  (before they were split between the README, the roadmap and the ADRs); an [upgrade guide](docs/how-to/upgrade.md)
  (the order, what each restart does to what is running, how to go back); a
  [backup and restore guide](docs/how-to/backup-and-restore.md), with its commands rehearsed against SQLite and
  Postgres and what a restored database does to the nodes; and [CONTRIBUTING.md](CONTRIBUTING.md).

### Changed

- **Roles:** a new role, `user` (`asp-user`), creates and runs its own sandboxes and sees no one else's.
  An `operator` can `exec` in other people's sandboxes only with the group `sandbox:exec-any` (as for
  destroying with `sandbox:destroy-any`): move the people and agents that only use sandboxes to
  `asp-user`. [Connect an IdP](docs/how-to/idp.md)
- **The packaged units read a file, not `Environment=` lines:** `ExecStart=… --config /etc/asp/agent.yaml`
  (or `server.yaml`), with the settings the unit used to carry in the file the package installs. The
  control plane's files belong to the group `asp-control-plane`, whose user reads them itself, so the
  package makes that user before it unpacks. The lab units (`scripts/systemd/`) do the same, with
  their settings in `scripts/systemd/lab/`. `asp doctor` no longer loads `/etc/asp/node-agent.env` by
  default (the node-agent reads its own file); `--env-file` still does for a unit that has one.
- `--local-net-key-dir` (`ASP_LOCAL_NET_KEY_DIR`) is a setting of the node-agent, so a file can set it.
- **Every setting has one name,** the flag's name in capitals with the `ASP_` prefix, in the three
  binaries; the old names still work and print a warning once. Booleans take `1/true/yes/on` and
  `0/false/no/off`, and anything else is an error.
- **Authentication is always on.** There is no fail-open mode: a lab that wants an open API says so
  with `ASP_INSECURE_OPEN_API=1`. Nodes authenticate with a certificate or a platform key.
- **Guest egress is enforced by default** on a node that has an egress proxy.
- A node that has enrolled takes no sandboxes until it registers.
- **The roadmap lists only what is ahead.** The history of the phases (`1a` … `3m`) moved to
  [docs/history.md](docs/history.md); what the code does not do is in [known limits](docs/reference/limitations.md).
- Node names are unique in the memory store too, as they are in Postgres; two API keys cannot share
  a secret (migration 022).
- Migrations 018 to 023: retained disks, the first boot, free disk, egress enforcement, the key secret
  index, the agent version.

### Security

- Workspaces: a `workspace_host_path` must be under the tenant's directory in a workspace root; the
  default share no longer exports `/`.
- The shared bootstrap token can enroll a new node or a revoked one, never re-key a live one; node
  certificates are valid for their node id only.
- The node-agent's local API takes a bearer token; loopback is no longer a credential.
- The egress proxy never connects to loopback, link-local or private destinations.
- Tenant API keys and IdP principals are confined to their tenant; callers are attributed from their
  credential, not from the request body; the IdP audience is required.
- The control plane refuses to start in production mode with keys in a temporary directory.
- Request bodies are bounded and stalled requests time out.

### Fixed

- The node-agent's help said `--agent-listen` has no authentication (it takes the bearer token of
  `--agent-token-file`) and that `--tap-auto` soft-fails (a TAP that cannot be created fails the start).
  `asp config show -h` prints its help. `--egress-enforce`'s help said the egress proxy needs it; the proxy
  denies either way and only the agent's warning depends on it.
- Many races and leaks around stop, resume, delete and agent restarts; see the pull requests of the
  [design review](https://github.com/luisgf/agent-sandbox-platform/issues/145).
