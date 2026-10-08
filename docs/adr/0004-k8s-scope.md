# ADR-0004: Alcance de Kubernetes

- **Estado:** Aceptada
- **Fecha:** 2026-09
- **Relacionados:** [0001-vmm-choice](0001-vmm-choice.md), [`../architecture.md`](../architecture.md), [instalar un nodo](../how-to/install-node.md)

## Contexto

Kubernetes es el default mental de muchos equipos para “correr cosas en Linux”. Tentaciones naturales en una plataforma de sandboxes:

1. Modelar **cada microVM como un Pod** (Kata, kubevirt, device plugin + CNI).
2. Meter el **node-agent dentro de un DaemonSet** y dejar que kube-scheduler decida nodos.
3. Usar NetworkPolicy/CNI como frontera de egress en lugar del proxy host-side.

Problemas que aparecen al mezclar dos schedulers (kube + nuestro control plane):

- **Propiedad de dispositivos:** `/dev/kvm`, TAP, vsock CIDs, sockets CH en `/run/asp` — ¿quién limpia tras un eviction?
- **Red:** CNI asume namespaces de Pod; nuestras microVMs usan TAP en el netns del host + nft `asp_egress`.
- **Estado y recuperación:** asignación de sandboxes a nodos, fencing y attestation viven en nuestro store Postgres, no en etcd de kube.
- **Ambigüedad de amenaza:** “está en un Pod” no implica frontera VMM; confunde auditorías.

Restricción FOSS/corp: el control plane **sí** puede desplegarse en K8s (Deployment + Service + Postgres), porque es un servicio HTTP/TLS stateless respecto al VMM.

## Decisión

**Kubernetes es opcional y se limita al despliegue del control plane.**

- Los **nodos de sandbox** son bare metal o VMs administradas con `node-agent` (systemd o proceso supervisado).
- **Los sandboxes no se programan como Kubernetes Pods.**
- El node-agent inicia y reconcilia microVMs **directamente** (CH spawn / FakeVMM), manteniendo explícita la frontera de aislamiento.
- Un CP en K8s habla con nodos externos por HTTPS + mTLS (enrollment PKI); no necesita que los nodos estén “dentro del cluster”.

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| Cada sandbox = Pod (Kata/KubeVirt) | Reutiliza tooling K8s | Dos schedulers; CNI vs TAP; recovery ambigua | Rechazada para MVP y horizonte cercano |
| Node-agent solo como DaemonSet | Rolling upgrades familiares | Sigue necesitando privilegios host; no elimina el reconciler propio | Aceptable como *empaquetado* del binario, no como modelo de sandbox |
| Todo bare-metal incluyendo CP | Ops homogénea | Pierde facilidad de HA/stateless del API | CP puede ser K8s **o** bare-metal; ambos OK |
| Nomad / Firecracker jailer custom | Menos magia K8s | Ecosistema menor en corp | No bloquea; fuera del MVP |

## Consecuencias

### Positivas

- Frontera de amenaza clara: microVM + node-agent privilegiado + CP API.
- El scheduler de capacidad (`vmm_profiles`, CPU/mem del nodo, work queue) vive en **nuestro** control plane (`GET /v1/nodes/{id}/work`, claim).
- No dependemos de device plugins, RuntimeClass ni CNI para el ciclo de vida de cada sandbox.
- CI/dry-run no necesita cluster.

### Negativas

- Operar nodos exige provisioning, upgrades, draining y observabilidad **fuera** del modelo Pod (documentado en bare-metal).
- Quienes esperen `kubectl get pods` por sandbox se decepcionan — hay que educar con el diagrama y este ADR.
- HA del node-agent (multi-réplica en el mismo host) no está resuelta vía kube; un host = un agent.

### Follow-ups

- Helm/manifests opcionales solo para `control-plane` + Postgres (no bloqueante).
- Cualquier propuesta “microVM-in-Pod” exige **ADR nuevo** + threat model actualizado; no se cuela por roadmap silencioso.

## Detalle de implementación en este repo

| Pieza | Cómo encaja |
|---|---|
| Control plane | `control-plane/cmd/api` — binario HTTP/TLS; `docker-compose.yml` solo trae Postgres |
| Node-agent | Proceso host; enroll `POST /v1/nodes/enroll`; register/heartbeat; `--reconcile` |
| Store | `nodes`, `sandboxes` — **no** objetos kube |
| Empaquetado | `make pack` / `scripts/pack-release.sh` — tarball de binarios, no chart de sandboxes |
| Docs ops | [instalar un nodo](../how-to/install-node.md) asume host KVM, no `kubectl apply` de VMs |

No hay manifiestos de “Sandbox CRD” en el árbol a propósito.

## Límites honestos / no-goals

- **No** hay modo “microVM dentro de Pod” en el MVP ni en fases 2.x.
- Desplegar el node-agent con un DaemonSet **no** convierte los sandboxes en Pods.
- NetworkPolicy de K8s **no** sustituye el forward proxy + nft del nodo.
- El proyecto no pretende ser un RuntimeClass ni un competidor de KubeVirt.

## Enmiendas

- **2026-10:** esta decisión descartó la sandbox-como-Pod con Kata y KubeVirt, pero no examinó el proyecto `agent-sandbox` de Kubernetes (SIG Apps), ni Kata con Cloud Hypervisor como `RuntimeClass`, ni Virtink o E2B como alternativas completas. Se reevaluará con una medición en el ADR-0013 (reservado, [#143](https://github.com/luisgf/agent-sandbox-platform/issues/143)). Entretanto, la comparación está en [ASP frente a Kubernetes](../concepts/asp-vs-kubernetes.md).

## Referencias cruzadas

- VMM: ADR-0001
- Red host-side: ADR-0002
- Roadmap “fuera de alcance”: [`../roadmap.md`](../roadmap.md) § Fuera de alcance inicial
- Diagrama (nodo bare metal / VM): [`../diagram.svg`](../diagram.svg)
