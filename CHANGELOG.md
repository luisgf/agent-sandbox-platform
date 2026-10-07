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
- A Postgres run of the API tests, and a parity script that holds the memory and Postgres stores to
  one contract.

### Changed

- **Every setting has one name,** the flag's name in capitals with the `ASP_` prefix, in the three
  binaries; the old names still work and print a warning once. Booleans take `1/true/yes/on` and
  `0/false/no/off`, and anything else is an error.
- **Authentication is always on.** There is no fail-open mode: a lab that wants an open API says so
  with `ASP_INSECURE_OPEN_API=1`. Nodes authenticate with a certificate or a platform key.
- **Guest egress is enforced by default** on a node that has an egress proxy.
- A node that has enrolled takes no sandboxes until it registers.
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

- Many races and leaks around stop, resume, delete and agent restarts; see the pull requests of the
  [design review](https://github.com/luisgf/agent-sandbox-platform/issues/145).
