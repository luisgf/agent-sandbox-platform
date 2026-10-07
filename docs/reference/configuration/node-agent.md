<!-- Generado por `make docs` a partir de los ajustes de node-agent/cmd/node-agent/main.go y de reference.go. No lo edites a mano: un test falla si no coincide con el código. -->
# Configuración del node-agent

Cada ajuste es una opción de la línea de órdenes y una variable de entorno `ASP_*`. Prioridad: **opción > variable de entorno > fichero de configuración > valor por defecto**. El fichero es `/etc/asp/agent.yaml` (más los `agent.yaml.d/*.yaml` que lo acompañan), o el que nombren `--config FILE` o `ASP_CONFIG`; la clave de un ajuste es su opción con guiones bajos (`control_plane_url`) y una clave que no es un ajuste es un error ([el fichero de configuración](../../how-to/config-file.md)). `node-agent --print-config` lista cada ajuste con su valor y de dónde sale, sin las credenciales; las marcadas con 🔒 son credenciales. Los booleanos se escriben igual en la opción, la variable y el fichero: `1`, `true`, `yes`, `on` / `0`, `false`, `no`, `off`.

Los ajustes de los otros programas: [plano de control](control-plane.md), [`asp`](cli.md) y [`asp-server`](asp-server.md).

## Plano de control, identidad del nodo y enrollment

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--control-plane-url` | `ASP_CONTROL_PLANE_URL` | `http://127.0.0.1:8080` | Control plane base URL. Nombres antiguos, que siguen valiendo con un aviso: `CONTROL_PLANE_URL`. |
| `--control-plane-ca` | `ASP_CONTROL_PLANE_CA` | — | PEM CA that signed the control plane's TLS certificate (enroll and API calls); default: `cert-dir/ca.crt`, then system roots. `cert-dir/ca.crt` is the CA that signs every node certificate, so with a remote control plane the agent warns: pass a CA that signs only the control plane's certificate. |
| `--enroll-url` | `ASP_ENROLL_URL` | — | Control-plane URL for `--enroll` when it differs from `--control-plane-url` (`ASP_MTLS_STRICT` serves enroll on a separate listener). |
| `--node-id` | `ASP_NODE_ID` | — | Node identifier. Nombres antiguos, que siguen valiendo con un aviso: `NODE_ID`. |
| `--endpoint` | `ASP_ENDPOINT` | — | Node callback endpoint advertised to control plane. Nombres antiguos, que siguen valiendo con un aviso: `NODE_ENDPOINT`. |
| `--enroll` | `ASP_ENROLL` | `false` | Perform bootstrap enrollment before register. |
| `--bootstrap-token` | `ASP_NODE_BOOTSTRAP_TOKEN` 🔒 | — | Shared bootstrap token for enrollment: enrolls a new node id or a revoked node, never re-keys an enrolled one. |
| `--enroll-token` | `ASP_NODE_ENROLL_TOKEN` 🔒 | — | Single-use enroll token from an admin (asp node enroll-token), used instead of `--bootstrap-token`; one pinned to this node re-keys it even when it is enrolled. |
| `--cert-dir` | `ASP_CERT_DIR` | `/var/lib/asp/node-certs` | Directory for node client certs. |
| `--mtls` | `ASP_MTLS` | `false` | Require mTLS client certs for control-plane calls. |
| `--api-key-file` | `ASP_NODE_API_KEY_FILE` | — | File with the platform-scoped API key this node sends to the control plane (also env `ASP_NODE_API_KEY`). Needed when the control plane is reached over plain HTTP, where there is no client certificate; the control plane refuses every node call without a credential. |

## Reconciliación y capacidad

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--reconcile` | `ASP_RECONCILE` | `false` | Poll control-plane work and drive VMM lifecycle. |
| `--reconcile-interval` | `ASP_RECONCILE_INTERVAL` | `2s` | Reconciler poll interval. |
| `--reconcile-workers` | `ASP_RECONCILE_WORKERS` | `4` | Sandboxes the reconciler starts or stops at once (1 with `--ch-api-socket`). |
| `--heartbeat-interval` | `ASP_HEARTBEAT_INTERVAL` | `30s` | Control-plane heartbeat interval. |
| `--capacity-cpu` | `ASP_CAPACITY_CPU` | `-1` | CPU cores offered to sandboxes: -1 detects, 0 is not enforced (the control plane overcommits CPU). |
| `--capacity-mem-mib` | `ASP_CAPACITY_MEM_MIB` | `-1` | Memory offered to sandboxes in MiB: -1 detects MemTotal minus max(1 GiB, 10%), 0 is not enforced. |
| `--max-sandboxes` | `ASP_MAX_SANDBOXES` | `0` | Maximum sandboxes on this node; 0 = no limit. |

## API local del agente (exec)

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--agent-listen` | `ASP_AGENT_LISTEN` | `127.0.0.1:9100` | Listen addr of the local API: exec in any sandbox, the egress policy check and the SSH-agent approvals. Plain HTTP; every route but GET `/healthz` needs the bearer token of `--agent-token-file`. Outside loopback the agent does not start unless `--insecure-agent-listen`. |
| `--agent-tls-listen` | `ASP_AGENT_TLS_LISTEN` | — | Listen addr for the control plane's mTLS exec API (e.g. `0.0.0.0:9443`) when the control plane runs on another host; uses the enrolled node certificate. |
| `--insecure-agent-listen` | `ASP_INSECURE_AGENT_LISTEN` | `false` | Allow `--agent-listen` on a non-loopback address (plain HTTP: the bearer token crosses the network in the clear; lab only). |
| `--agent-token-file` | `ASP_AGENT_TOKEN_FILE` | — | File holding the secret that guards the local API on `--agent-listen` (bearer token). Created, 0600, if missing. A control plane on this host reads the same file (`ASP_AGENT_TOKEN_FILE`) and must be able to read it (if it does not run as root, create the file first, mode 0640 with its group). Default `/var/lib/asp/agent.token`, or a temporary file when that directory is not writable (dry-run labs). |

## Hipervisor, discos y workspaces

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--ch-binary` | `ASP_CH_BINARY` | `cloud-hypervisor` | Cloud-hypervisor binary path (spawned per sandbox when not using `--ch-api-socket`). Nombres antiguos, que siguen valiendo con un aviso: `CLOUD_HYPERVISOR_BIN`. |
| `--ch-socket-dir` | `ASP_CH_SOCKET_DIR` | `/run/asp` | Directory for per-sandbox CH API sockets (`ch-{sandboxID}.sock`). The agent creates it 0700 and tightens an existing one that is not a system directory (`/run`, `/tmp…`): the sockets inside give authority over every sandbox (root exec in the guest, identity tokens, virtiofsd). Nombres antiguos, que siguen valiendo con un aviso: `CH_SOCKET_DIR`. |
| `--ch-api-socket` | `ASP_CH_API_SOCKET` | — | Optional shared CH `--api-socket` (legacy/debug); empty = per-sandbox spawn via `--ch-socket-dir`. Nombres antiguos, que siguen valiendo con un aviso: `CH_API_SOCKET`. |
| `--guest-kernel` | `ASP_GUEST_KERNEL` | `/opt/sandbox/vmlinux` | Kernel (an uncompressed vmlinux) every VM boots. A test with another guest needs no change to `/opt/sandbox`; the attestation (`--print-measurement`) measures this file and `--guest-rootfs`. |
| `--guest-rootfs` | `ASP_GUEST_ROOTFS` | `/opt/sandbox/rootfs.img` | Base rootfs image every sandbox's private disk is copied from; never booted itself. |
| `--guest-verify` | `ASP_GUEST_VERIFY` | `auto` | Check the guest kernel and base image against a SHA256SUMS next to them (asp image pull installs one): auto (refuse to boot from a file the sums list with another digest), on (also refuse one no sums list) or off. The sandbox fails with the reason; a digest is cached while the file does not change. |
| `--guest-ready-timeout` | `ASP_GUEST_READY_TIMEOUT` | `2m0s` | Wait up to this long for pod-daemon in a new VM to answer before reporting running; a guest that never does fails the start (0: report running as soon as the VMM is up; ignored with `--dry-run`). |
| `--disk-dir` | `ASP_DISK_DIR` | `/var/lib/asp/disks` | Per-sandbox rootfs copies (`rootfs-{id}.img`). A stop keeps the copy when the control plane keeps stopped sandboxes (ADR-0012); a delete, or a control plane that does not, removes it. Copies no sandbox owns are removed after a poll. Ignored with `--dry-run`. |
| `--disk-min-free-mib` | `ASP_DISK_MIN_FREE_MIB` | `-1` | Refuse to clone or resume a sandbox disk when `--disk-dir` has less free space (MiB). -1: twice the base image's size; 0: do not check. |
| `--stop-grace` | `ASP_STOP_GRACE` | `15s` | A stop asks the guest to power off (sync; systemctl poweroff, through pod-daemon) and waits up to this long for the VM to exit before stopping it hard (0: stop hard at once; ignored with `--dry-run`). |
| `--workspace-root` | `ASP_WORKSPACE_ROOTS` | `/srv/asp/workspaces` | Comma-separated directories a sandbox's workspace may live under: a workspace must be inside `<root>/<tenant>/` (symbolic links resolved). The workspace path comes from the sandbox spec, so without this any caller could export the node's disks and keys; with no root that exists, no sandbox can have a workspace. |
| `--virtiofsd-bin` | `ASP_VIRTIOFSD_BIN` | `virtiofsd` | Rust virtiofsd binary; started per sandbox only when workspace_host_path is set. Nombres antiguos, que siguen valiendo con un aviso: `VIRTIOFSD_BIN`. |
| `--virtiofsd-sandbox` | `ASP_VIRTIOFSD_SANDBOX` | — | Virtiofsd `--sandbox` mode: chroot (confine the daemon to the workspace), namespace or none. Default chroot when running as root, none otherwise. |
| `--pod-daemon-sock` | `ASP_POD_DAEMON_SOCK` | — | Unix socket path for pod-daemon (dry-run / local fallback). |
| `--pod-daemon-port` | `ASP_POD_DAEMON_PORT` | `26500` | Guest vsock/TCP port for pod-daemon HTTP (CH hybrid CONNECT). |
| `--reap-leftovers` | `ASP_REAP_LEFTOVERS` | `on` | At start, remove what a previous node-agent left on this host: cloud-hypervisor and virtiofsd processes of `--ch-socket-dir`, its per-sandbox sockets, asp-* TAPs, wg-asp-* tunnels and their routing (not the rootfs copies in `--disk-dir`: the reconciler removes the ones no sandbox owns). on \| report (log, remove nothing) \| off; `--dry-run` only reports. |

## Confinamiento de las microVMs

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--vm-confine` | `ASP_VM_CONFINE` | `auto` | Run each microVM and its virtiofsd in a transient systemd service (asp-vm-&lt;id&gt;, asp-vm-&lt;id&gt;-fs) with its own cgroup and resource limits: auto (when this host can: root, systemd; otherwise as children of this process, and it says why), on (refuse to start if it cannot) or off (children of this process, as before). |
| `--vm-survive-restart` | `ASP_VM_SURVIVE_RESTART` | `true` | A confined microVM keeps running when the agent stops or restarts, and the next agent process takes it over (default; each running VM leaves a record in `<ch-socket-dir>/state/`). `=false` binds each VM's service to the agent's, so systemd stops the VMs with it, as before. |
| `--vm-unprivileged` | `ASP_VM_UNPRIVILEGED` | `auto` | Run each microVM's Cloud Hypervisor as an unprivileged user of its own, with no capabilities and a service that cannot open IP sockets or write outside its own files: auto (when this host can, which it checks by opening, as that user, what the VMM needs; otherwise the VMM stays root and it says why), on (refuse to start if it cannot) or off (root, as before). Needs `--vm-confine`; virtiofsd stays root (`--virtiofsd-sandbox`). |
| `--vm-uid-base` | `ASP_VM_UID_BASE` | `1879048192` | First user id of the microVMs: the one with guest CID n runs as this plus n. Pick a range that no user, container tool or other agent uses. |
| `--vm-run-dir` | `ASP_VM_RUN_DIR` | — | Directory with one subdirectory per microVM, owned by the VM's user, where its VMM keeps its sockets; mode 0711. Default: `--ch-socket-dir` with -vm appended (`/run/asp-vm`). |
| `--vm-slice` | `ASP_VM_SLICE` | `asp-vms.slice` | Systemd slice of the microVM services. |
| `--vm-memory-overhead-mib` | `ASP_VM_MEMORY_OVERHEAD_MIB` | `256` | Memory added to the guest's for the VMM's own use, in the unit's MemoryMax. |
| `--vm-cpu-overhead-percent` | `ASP_VM_CPU_OVERHEAD_PERCENT` | `50` | Percent of one CPU added to the guest's vCPUs for the VMM's own threads, in the unit's CPUQuota. |
| `--vm-tasks-max` | `ASP_VM_TASKS_MAX` | `1024` | Most processes and threads of one microVM service. |

## Red de las VMs y local-net

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--guest-subnet` | `ASP_GUEST_SUBNET` | `10.200.0.0/16` | Guest pool: each TAP gets its own `/30` from it; also the nft `--egress-nft-redirect` match. |
| `--tap-auto` | `ASP_TAP_AUTO` | `false` | Create and delete an `asp-{shortid}` TAP, with its own `/30` of `--guest-subnet`, around VMM Start/Stop, and give the guest its hostname, resolver and HTTP_PROXY on its kernel command line. A TAP that cannot be created fails the start (only `--dry-run` goes without TAPs). |
| `--local-net-key-dir` | `ASP_LOCAL_NET_KEY_DIR` | — | Where the per-sandbox WireGuard node keys of on-demand local-net are kept (default `/var/lib/asp/local-net`; under the temp dir with `--dry-run`). |
| `--local-net-dial` | `ASP_LOCAL_NET_DIAL` | — | `host[:port]` laptops dial for this node's local-net tunnels (default: the control plane's `ASP_LOCAL_NET_DIAL`). |

## Salida a Internet (egress)

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--egress-enforce` | `ASP_EGRESS_ENFORCE` | `false` | Return 403 on `/v1/internal/egress-check` denials; required intent for `--egress-proxy-listen`. |
| `--egress-proxy-listen` | `ASP_EGRESS_PROXY_LISTEN` | — | Optional HTTP forward proxy listen (e.g. `:8888`); guests set HTTP_PROXY to host TAP IP:port. |
| `--egress-dns-sink` | `ASP_EGRESS_DNS_SINK` | — | Optional UDP DNS sink (e.g. `:5353`) that NXDOMAIN non-allowlisted names. With the nft redirect and `--nft-dns-action=redirect` it starts by itself on `:5353` when not named. |
| `--egress-allow-cidr` | `ASP_EGRESS_ALLOW_CIDRS` | — | Comma-separated private networks (CIDR or address) the egress proxy may connect to, on top of the public internet. The proxy checks the address after resolving the name. Loopback, link-local, multicast, this node's own addresses and the guests' network are never reachable, whatever a tenant allows. |
| `--egress-mitm` | `ASP_EGRESS_MITM` | `false` | ENABLE CONNECT TLS bump (corp caution; default off). |
| `--egress-mitm-ca` | `ASP_EGRESS_MITM_CA` | — | Optional path to MITM CA PEM (generate/load); used only with `--egress-mitm` / `ASP_EGRESS_MITM=1`. |
| `--egress-nft-redirect` | `ASP_EGRESS_NFT_REDIRECT` | `auto` | Force guest HTTP(S)+DNS through the egress proxy and sink with nftables and drop the rest. Default: on when `--egress-proxy-listen` is set and this is not `--dry-run`; `--egress-nft-redirect=false` turns it off (HTTP_PROXY is then voluntary). Nombres antiguos, que siguen valiendo con un aviso: `--nft-egress-redirect`, `ASP_NFT_EGRESS_REDIRECT`. |
| `--nft-egress-mode` | `ASP_NFT_EGRESS_MODE` | — | What a node that cannot apply the nft rules does: enforce (refuse to start; the default) or soft (start without egress enforcement; the default with `--dry-run`). The node reports egress_enforced=true when it registers only with the proxy listening and the rules applied in enforce mode. |
| `--nft-dns-action` | `ASP_NFT_DNS_ACTION` | `redirect` | Guest DNS handling: redirect (to `--egress-dns-sink` port) \| drop. |
| `--nft-http-ports` | `ASP_NFT_HTTP_PORTS` | `80,443` | Comma-separated guest TCP ports redirected to egress proxy. |

## Identidad de las sandboxes y agente SSH

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--host-vsock` | `ASP_HOST_VSOCK` | `false` | guest→host SSH(26501)+identity(26502): AF_VSOCK/unix lab + per-sandbox CH hybrid `{vsock}_{port}`. |
| `--host-vsock-dir` | `ASP_HOST_VSOCK_DIR` | — | If set, use unix sockets under this dir instead of AF_VSOCK (lab). |
| `--identity-listen` | `ASP_IDENTITY_LISTEN` | — | Unix path (.sock) or TCP addr for guest OIDC identity proxy. |
| `--default-sandbox-id` | `ASP_SANDBOX_ID` | — | Sandbox for identity requests without X-ASP-Sandbox-ID on listeners not bound to a sandbox; only with `--insecure-identity-sandbox-header` (dry-run). |
| `--insecure-identity-sandbox-header` | `ASP_INSECURE_IDENTITY_SANDBOX_HEADER` | `false` | Let identity listeners not bound to a sandbox (`--identity-listen`, global `--host-vsock`) take the sandbox from the client's X-ASP-Sandbox-ID, then `--default-sandbox-id`; anyone who reaches them can mint any sandbox's token (lab only). |
| `--ssh-agent-bridge` | `ASP_SSH_AGENT_BRIDGE` | — | Unix socket path for SSH agent bridge (proxies SSH_AUTH_SOCK or FakeAgent). |
| `--ssh-agent-confirm` | `ASP_SSH_AGENT_CONFIRM` | `auto` | Require POST `/v1/internal/ssh-agent/approve` with the signing sandbox's sandbox_id before each SignRequest (one-shot TTL); default on in multi-user. |
| `--insecure-ssh-agent-global-approvals` | `ASP_INSECURE_SSH_AGENT_GLOBAL_APPROVALS` | `false` | With `--ssh-agent-confirm`, accept approvals without sandbox_id; they unlock one sign on listeners that cannot tell guests apart (`--ssh-agent-bridge`, global `--host-vsock`), from whichever guest asks first (lab only). |
| `--ssh-agent-sock-template` | `ASP_SSH_AGENT_SOCK_TEMPLATE` | — | Per-sandbox SSH agent upstream path template (`{owner_sub}/{sandbox_id}/{id}`); missing → FakeAgent. |
| `--multi-user` | `ASP_MULTI_USER` | `false` | Multi-user profile: SSH confirm default-on + prefer scoped agent socks. Nombres antiguos, que siguen valiendo con un aviso: `ASP_IDP_REQUIRED`. |

## Observabilidad

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--metrics-listen` | `ASP_METRICS_LISTEN` | — | Serve Prometheus metrics on this address (GET `/metrics`), e.g. `127.0.0.1:9102`. No authentication, so loopback only unless `--insecure-obs-listen`. Off by default. |
| `--pprof-listen` | `ASP_PPROF_LISTEN` | — | Serve Go runtime profiles on this address (`/debug/pprof/`), e.g. `127.0.0.1:6060`. No authentication, so loopback only unless `--insecure-obs-listen`. Off by default. |
| `--insecure-obs-listen` | `ASP_INSECURE_OBS_LISTEN` | `false` | Allow `--metrics-listen` and `--pprof-listen` on a non-loopback address (no authentication; put a proxy that authenticates in front). |

## Laboratorio, diagnóstico y acciones

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--dry-run` | `ASP_DRY_RUN` | `false` | Use FakeVMM and skip real CH. Nombres antiguos, que siguen valiendo con un aviso: `DRY_RUN`. |
| `--config` | `ASP_CONFIG` | — | Settings file (YAML, keys named like the flags); its drop-ins are read from `<file>.d/*.yaml`. Default: `/etc/asp/agent.yaml` when it exists. Flags and environment variables win over it. |
| `--print-config` | — | `false` | Print the effective configuration, with where each value came from (flag, environment, file, default), and exit. |
| `--doctor` | — | `false` | Check this host and this configuration (KVM, hypervisor, virtiofsd, guest images, disk, nftables, TAPs, clock, the control plane) and print what is wrong and how to fix it; exit 1 when a check failed. |
| `--doctor-json` | — | `false` | With `--doctor`, print the report as JSON. |
| `--print-measurement` | — | `false` | Print the digests of the kernel and base image and the hypervisor version this node attests, as an entry for the control plane's `ASP_ATTEST_ALLOWED_IMAGES`, and exit. |
| `--reap-only` | — | `false` | Remove those leftovers and exit without registering (systemd ExecStopPost); refused while a node-agent runs with this `--ch-socket-dir`. |
| `--version` | — | `false` | Print which build this is and exit. |

## Obsoletos

| Opción | Variable | Por defecto | Qué hace |
|---|---|---|---|
| `--guest-ssh-agent-auto` | `ASP_GUEST_SSH_AGENT_AUTO` | — | Deprecated, no effect: it only logged what the guest image does; the image decides whether its ssh-agent-vsock service runs. |

## Variables sin opción

Se leen del entorno y no tienen opción ni clave en el fichero.

| Variable | Por defecto | Qué hace |
|---|---|---|
| `ASP_ATTEST_KEY` | `$TMPDIR/asp-attest-key.pem`, solo en un lab | Clave ECDSA P-256 (PEM) con la que el nodo firma sus atestaciones de arranque cuando no usa la de su certificado; se crea si no existe. El plano de control tiene que fiarse de su clave pública (el mismo fichero en un lab de un host, o `ASP_ATTEST_TRUSTED_PUBS`). Un nodo de producción sin esta variable firma solo con la clave de su certificado y no crea ninguna clave temporal |
| `ASP_ALLOW_TMP_KEYS` | — | `1`: un nodo de producción arranca aunque `--cert-dir`, `ASP_ATTEST_KEY` o `--egress-mitm-ca` (con `--egress-mitm`) estén en un directorio temporal (`/tmp`, `/var/tmp`, `/dev/shm`, `$TMPDIR`), que un reinicio vacía. Es de producción un nodo sin `--dry-run` con `--agent-tls-listen`, `--mtls` o un certificado en `--cert-dir`: ahí, sin esta variable, no arranca (código 2). En un lab solo avisa |
| `ASP_NODE_API_KEY` 🔒 | — | La API key (ámbito `platform`) que el nodo manda al plano de control, para quien no quiere un fichero; `--api-key-file` gana. Hace falta con un plano de control por HTTP plano, donde no hay certificado de cliente |
| `ASP_EGRESS_ALLOWLIST_JSON` | ninguna: deny-default | Política de egress de todo origen que no es una sandbox conocida, en JSON (`{"mode":"deny-default","rules":[{"host_pattern":"*.example.com","port":443,"enabled":true}]}`). Una sandbox conocida usa la política de su tenant, que llega con el trabajo; un JSON que no se entiende se ignora |
| `ASP_EGRESS_MAX_BODY` | `8388608` (8 MiB) | Bytes máximos del cuerpo de una petición HTTP por el proxy de egress; más es un 413. Las respuestas pasan enteras |
| `ASP_NFT_SCRIPT` | el script que lleva el binario | Ruta de un script de nftables propio que sustituye al embebido. Si no existe, el nodo falla en vez de aplicar otro |
| `ASP_NFT_TABLE` | `asp_egress` | Nombre de la tabla de nftables del redirect de egress |
| `ASP_EGRESS_PROXY_IP` | `10.200.0.1` | Informativa: sale en la nota que imprime el script de nftables. Las reglas redirigen a un puerto local y no usan la IP |
| `ASP_EGRESS_PROXY_PORT` | `8888` | El node-agent toma el puerto de `--egress-proxy-listen` (8888 si no lo da): esta variable solo vale para el script de nftables ejecutado a mano |
| `ASP_EGRESS_DNS_SINK_IP` | el valor de `ASP_EGRESS_PROXY_IP` | Informativa, como `ASP_EGRESS_PROXY_IP` |
| `ASP_EGRESS_DNS_SINK_PORT` | `5353` | El node-agent toma el puerto de `--egress-dns-sink` (5353 si no lo da): esta variable solo vale para el script de nftables ejecutado a mano |
| `ASP_FENCE_ENDPOINT` | — | Se ignora, con un aviso: un nodo no elige cómo se le apaga. El destino de fencing lo fija un operador en el plano de control (`asp node fence set`) |
| `ASP_FENCE_TOKEN` | — | Se ignora, con un aviso, por lo mismo que `ASP_FENCE_ENDPOINT` |
| `SSH_AUTH_SOCK` | — | El agente SSH del usuario al que se conecta el puente (`--ssh-agent-bridge`) cuando la sandbox no tiene un socket propio. No es de ASP. |
