# ADR-0001: VMM por defecto — Cloud Hypervisor

- **Estado:** Aceptada
- **Fecha:** 2026-09
- **Relacionados:** [0002-networking](0002-networking.md), [0004-k8s-scope](0004-k8s-scope.md), [`../bare-metal-ch.md`](../bare-metal-ch.md), [`../roadmap.md`](../roadmap.md) §1g–2a

## Contexto

Agent Sandbox Platform necesita una frontera de aislamiento **más fuerte que un contenedor** para workloads de agentes (código no confiable, egress controlado, identidad host-held). Requisitos duros del diseño:

1. **Arranque rápido** y densidad razonable en Linux server (y un camino creíble hacia desktop/local).
2. **virtio** (blk, net) y **vsock** (canal host↔guest sin depender de TCP en la red del host).
3. API/CLI moderna, mantenible y **FOSS** (sin dependencias de hipervisores propietarios).
4. Capacidad de **spawn por sandbox** (un proceso VMM por microVM) para aislar fallos y sockets.
5. Perfil de **alta densidad multi-tenant** como opción futura, sin bloquear el default.

Restricciones corporativas / lab: muchos entornos solo tienen KVM; nested virt es aceptable en lab pero no es el target de producción. CI y el box compartido **no** tienen `/dev/kvm`, así que el node-agent debe poder correr con `FakeVMM` (`--dry-run`) sin mentir sobre lo que es “bare-metal”.

## Decisión

**Cloud Hypervisor (CH) es el VMM por defecto.**

El node-agent habla con CH vía su HTTP API sobre Unix socket:

- Modo **por defecto (productivo):** spawnea un `cloud-hypervisor --api-socket /run/asp/ch-{sandboxID}.sock` por cada `Start` (`--ch-socket-dir`, default `/run/asp`).
- Modo **legacy/debug:** `--ch-api-socket` apunta a un CH pre-arrancado (un VM a la vez).
- Modo **lab/CI:** `--dry-run` → `FakeVMM` (misma interfaz `Start`/`Stop`/`Pause`, sin proceso CH).

**Firecracker** queda como **perfil alternativo documentado**, no implementado como default. Sirve para densidades server-side donde el modelo de dispositivos reducido sea una ventaja; el scheduler usará `vmm_profile` explícito, no un “auto” opaco.

## Alternativas consideradas

| Alternativa | Pros | Contras | Por qué no |
|---|---|---|---|
| **Firecracker como default** | Muy denso, superficie pequeña, maduro en multi-tenant | Menos dispositivos (complicaría desktop/virtiofs/experimentos); API distinta | Densidad no es el cuello del MVP; vsock+virtio-pci de CH encajan mejor |
| **QEMU/KVM clásico** | Ubicuo, rich device model | Superficie enorme, arranque más lento, ops más pesada | Exceso de superficie para sandbox de agente |
| **Solo contenedores (runc/gVisor)** | Simple de empaquetar | Frontera más débil; no cumple el threat model “microVM first” | Contenedores dentro del guest pueden existir como packaging, no como frontera |
| **Kata / libkrun / cloud-hypervisor embebido en K8s** | Ecosistema | Mezcla schedulers; ver ADR-0004 | Los sandboxes no se programan como Pods |

## Consecuencias

### Positivas

- Una sola implementación VMM madura en `node-agent/internal/vmm/` (`cloudhypervisor.go`, `runner.go`, `fake.go`).
- Interfaz interna (`VMM` + `MicroVM`) permite FakeVMM en CI y CH en bare-metal sin ramificar el reconciler.
- Spawn por sandbox aísla sockets y procesos; un hang de CH no tumba el node-agent entero.
- vsock hybrid (host UDS ↔ guest AF_VSOCK) encaja con el dataplane exec (puerto **26500**) y host-vsock (**26501/26502**).

### Negativas / costes

- Hay que **fijar y validar** versión de CH + kernel + rootfs en ops (`bare-metal-ch.md`); no hay tracking automático de upstream.
- CH expone más dispositivos que Firecracker → política de “mínimo necesario” en `MicroVMConfig` (kernel, rootfs, TAP, vsock; sin gadgets extra).
- Nested virt en lab degrada densidad/latencia; hay que documentarlo como límite, no como bug.

### Follow-ups

- Perfil Firecracker (Fase 4 del roadmap): adaptador + benchmarks, sin prometer paridad inmediata.
- Pin de release CH en el tarball de `make pack` / checklist bare-metal.

## Detalle de implementación en este repo

| Pieza | Ruta / flag |
|---|---|
| Interfaz VMM | `node-agent/internal/vmm/vmm.go` (`MicroVMConfig`, `Start`/`Stop`/`Pause`) |
| Cliente CH | `node-agent/internal/vmm/cloudhypervisor.go` — `vmm.ping`, `vm.create`, `vm.boot`, `vm.delete` |
| Spawn por sandbox | `NewSpawningCloudHypervisor(bin, socketDir)` vía `--ch-socket-dir` / `CH_SOCKET_DIR` |
| FakeVMM | `node-agent/internal/vmm/fake.go` + `--dry-run` / `DRY_RUN=1` |
| Binario | `--ch-binary` / `CLOUD_HYPERVISOR_BIN` (default `cloud-hypervisor`) |
| Shared socket | `--ch-api-socket` / `CH_API_SOCKET` (legacy) |
| Reconciler | `node-agent/internal/reconciler/` — arma `MicroVMConfig` con `TapDevice`, `VsockCID`, kernel/rootfs |
| Assets guest | `/opt/sandbox/vmlinux`, `/opt/sandbox/rootfs.img` (convención ops; ver bare-metal) |
| Guía ops | [`../bare-metal-ch.md`](../bare-metal-ch.md) §2, §5 |

Ejemplo (bare-metal, sin dry-run):

```bash
node-agent \
  --control-plane-url=https://cp:8443 --mtls --cert-dir=/var/lib/asp/node-certs \
  --node-id=node-a --reconcile --ch-socket-dir=/run/asp \
  --ch-binary=/usr/local/bin/cloud-hypervisor \
  --tap-auto --host-vsock
```

## Límites honestos / no-goals

- **Software FakeVMM ≠ KVM.** Los smokes dry-run demuestran el plano de control y el dataplane unix; no demuestran aislamiento de hipervisor.
- **No Windows guests.** Solo Linux microVMs.
- **No paridad Firecracker** en el MVP; el campo `vmm_profile` existe en el modelo (`store.Sandbox`) pero el node-agent actual anuncia `cloud-hypervisor` en enroll.
- **No** empaquetar microVMs como Kubernetes Pods (ADR-0004).
- Attestation de boot es **software-signed** (Fase 2c), no TPM/SEV hardware.

## Referencias cruzadas

- Arquitectura: [`../architecture.md`](../architecture.md)
- Diagrama: [`../diagram.svg`](../diagram.svg) / [`../diagram.mmd`](../diagram.mmd)
- Roadmap fases 1g (multi-socket), 2a (TAP/host-vsock): [`../roadmap.md`](../roadmap.md)
- Smoke sin KVM: [`../mvp-smoke.md`](../mvp-smoke.md)
