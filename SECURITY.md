# Security policy

ASP is a sandbox. A way to break its isolation is the most important kind of bug it can have, so thank you for looking.

## Reporting a vulnerability

Please **do not open a public issue**. Report privately through GitHub: [**Report a vulnerability**](https://github.com/luisgf/agent-sandbox-platform/security/advisories/new) (Security tab → *Report a vulnerability*).

Include what you can of:

- the affected component and commit;
- how the node ran: dry-run (`FakeVMM`) or KVM + Cloud Hypervisor, and nftables in `soft` or `enforce` mode;
- steps to reproduce or a proof of concept;
- the impact you observed.

Prove impact with a benign canary (for example, a marker file written on the host), not a destructive payload, and test only against systems you own.

This is a small project maintained on a best-effort basis. You will get a reply in the advisory thread. A fix is coordinated with you before anything is disclosed, and you are credited in the advisory unless you prefer not to be.

## Supported versions

The latest release and the `main` branch are supported, and a fix lands on `main` first. There are no releases yet (the first will be 0.1.0, see [CHANGELOG.md](CHANGELOG.md)), so today only `main` is.

## Scope

In scope is anything that breaks a boundary ASP claims to enforce:

- Escaping the microVM, or code in the guest reaching the host, the hypervisor API socket or another sandbox.
- Bypassing deny-by-default egress (forward proxy, DNS sink, nftables in `enforce` mode), or one sandbox's egress policy applying to another.
- Long-lived credentials reaching the guest: the operator's SSH keys, control-plane tokens, or OIDC tokens beyond their intended lifetime or audience.
- Spoofing `owner_sub`, or acting on a sandbox you do not own, through the control-plane API.
- Impersonating a node or the control plane (mTLS enrollment, bootstrap tokens, PKI).
- `--local-net` changing the host's default route, or reaching the local network from a session that did not opt in.

Out of scope are the limits the project already documents in [Status and known limits](README.md#status-and-known-limits):

- Dry-run mode (`FakeVMM`). It exercises the control plane and provides no isolation.
- nftables in `soft` mode, which tolerates missing privileges by design.
- Software attestation (`ASP_ATTEST_KEY`). It is not TPM/SEV.
- Lease fencing through the stub `FenceProvider`. It is not BMC STONITH.
- Vulnerabilities in Cloud Hypervisor, the Linux kernel or other upstream projects. Please report those upstream; if ASP configures them insecurely, that part is in scope here.
