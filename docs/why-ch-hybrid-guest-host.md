# Por qué / Qué ganamos — CH hybrid guest→host (SSH agent + identity)

## Por qué

Cloud Hypervisor (y Firecracker) usan **hybrid vsock**: un UDS muxer en el host
(`--vsock cid=…,socket=/run/asp/vsock-{id}.sock`).

| Dirección | Mecánica | ASP |
|---|---|---|
| **Host → guest** | `connect(muxer)` + `CONNECT <port>\n` → `OK …\n` | `HybridVsockDialer` (exec / pod-daemon) ✅ |
| **Guest → host** | Guest dials AF_VSOCK CID **2**:port; VMM dials host UDS **`{muxer}_{port}`** | Antes: solo `AF_VSOCK Listen` ❌ |

En bare-metal (ncc1701d) se demostró: host→guest exec OK; guest
`vsock-ssh-agent-proxy` → CID 2:26501 → **`connection reset by peer`**.
`node-agent --host-vsock` con `vsock.Listen(26501)` **no** recibe esas
conexiones del muxer híbrido de CH: el VMM nunca las entrega a AF_VSOCK del
host; espera un listener en `/run/asp/vsock-{id}.sock_26501`.

Sin ese listener, CH/Firecracker responden RST al guest.

## Qué ganamos

- Por sandbox, al `Start` del reconciler: listeners unix en
  `{VsockPath}_26501` (SSH) y `{VsockPath}_26502` (identity).
- Mismo handler que el path global: `sshagent.ServeConnWithConfirm` /
  `IdentityHandler`.
- `DetachSandbox` al stop limpia sockets.
- AF_VSOCK / `--host-vsock-dir` siguen disponibles (lab, VMM que sí bridgee
  AF_VSOCK); dry-run FakeVMM con `PodDaemonUnix` **no** adjunta hybrid.
- Guest sin cambios: sigue dialando CID 2:26501.

## Protocolo (referencia)

Documentación CH [`docs/vsock.md`](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md):

```bash
# Host (nosotros):
socat - UNIX-LISTEN:/tmp/ch.vsock_1234

# Guest:
socat - VSOCK-CONNECT:2:1234
```

Firecracker: misma convención `uds_path_PORT`.

## Límites honestos

- Un listener por sandbox×puerto (no un solo AF_VSOCK global para CH).
- Hay que adjuntar **antes** de que el guest dialee; el reconciler adjunta
  antes de `Engine.Start`.
- Si el path del muxer cambia (snapshot restore / override), hay que
  re-Attach (no implementado).
- Identity HTTP por hybrid no añade autenticación extra: el guest sigue
  siendo untrusted; headers `X-ASP-Sandbox-ID` no son prueba de identidad.
- Verify bare-metal requiere imagen guest con `vsock-ssh-agent-proxy` y
  node-agent desplegado con este cambio.

## Cómo probar (lab)

```bash
cd node-agent && go test ./internal/hostvsock/ -count=1
# Ver TestHybridAttachSSHAgentFake: listen {path}_26501 → REQUEST_IDENTITIES → type 12
```

Bare-metal: ver `scripts/guest-vsock-notes.md` § Guest→host hybrid + demo script.
