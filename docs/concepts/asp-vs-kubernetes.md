# ASP frente a Kubernetes

Para quien llega con Kubernetes en la cabeza: qué es una sandbox de ASP en esos términos, qué hace Kubernetes (con `agent-sandbox`, Kata, KubeVirt…) que ASP reimplementa, qué hace ASP que Kubernetes no hace de serie, y cuándo conviene cada uno. No es una página de marketing: ASP es un proyecto pequeño sin releases todavía, y en lo que Kubernetes cubre bien, Kubernetes lo cubre mejor.

## El mismo vocabulario

| En Kubernetes | En ASP |
|---|---|
| Pod | **sandbox**: una microVM de Cloud Hypervisor, con su disco privado |
| Namespace | **tenant** |
| kubelet + runtime (containerd, un shim de Kata) | **node-agent** + Cloud Hypervisor |
| kube-scheduler | el planificador del **plano de control** (`spread` o `binpack`, con CPU, memoria y huecos; 503 con los motivos si nada cabe) |
| etcd | **Postgres** (o SQLite en un solo host) |
| `kubectl`, una API declarativa | `asp` y una API REST: se pide una sandbox, no se declara un estado |
| ServiceAccount token | el **token OIDC de workload** que acuña el plano de control para esa sandbox |
| NetworkPolicy | la **política de egress por tenant**, que aplican un proxy, un sumidero de DNS y nftables en el nodo |
| `cordon`, `drain` | `asp node cordon`, `uncordon` (no hay `drain`: las sandboxes no se mueven) |
| RuntimeClass (`kata`, `gvisor`) | no existe: toda sandbox es una microVM |
| PersistentVolumeClaim | el **disco local retenido** de la sandbox, fijado a su nodo |

## Qué es cada cosa

**ASP** es una plataforma autoalojada para ejecutar código no confiable de agentes de IA, una microVM por sandbox, con una **sesión** como unidad de producto (un sandbox, muchos `exec`, parar y reanudar sin perder el disco) y dos cosas que no se delegan en el guest: las **credenciales** (el agente SSH del host y los tokens OIDC acuñados en el servidor, por vsock) y el **egress** (deny-by-default, por tenant, con el origen identificado por IP).

**Kubernetes + `agent-sandbox`** es el proyecto de SIG Apps que ofrece, sobre Kubernetes, CRDs (`Sandbox`, `SandboxTemplate`, `SandboxClaim`, `SandboxWarmPool`) para cargas aisladas con estado, como las de un agente: identidad estable, almacenamiento persistente, pausa y reanudación, pools precalentados, y SDK en Go y Python. El aislamiento lo delega en un *runtime* por `RuntimeClass`: gVisor (un kernel en espacio de usuario) o Kata Containers (una VM por pod, con QEMU, Cloud Hypervisor, Firecracker…).

**KubeVirt** y **Virtink** corren VMs completas como recursos de Kubernetes (KubeVirt con QEMU y libvirt dentro de un pod `virt-launcher`, Virtink con Cloud Hypervisor); no están pensados para sandboxes de agentes, pero son la otra forma de «microVM en Kubernetes». **E2B** publica su infraestructura (Apache-2.0): una microVM Firecracker por sandbox, restauradas desde snapshots en vez de arrancar un kernel, con un orquestador por nodo.

## Comparación

Lo de ASP es lo que hace hoy el código; lo del resto, lo que dicen sus proyectos (fuentes abajo). Una celda sin cifra es una cifra que este proyecto no ha medido.

| | ASP | Kubernetes + `agent-sandbox` | KubeVirt | Virtink | E2B (infra abierta) |
|---|---|---|---|---|---|
| **Unidad de aislamiento** | microVM de Cloud Hypervisor; el VMM corre como un usuario sin privilegios por VM | Pod con `RuntimeClass`: gVisor, o Kata (una VM por pod) | VM QEMU/libvirt dentro de un pod | VM de Cloud Hypervisor dentro de un pod | microVM de Firecracker |
| **Planificador** | propio, por capacidad | kube-scheduler | kube-scheduler | kube-scheduler | la API decide el nodo; un orquestador por nodo ejecuta |
| **Reiniciar el agente del nodo** | las VMs confinadas siguen y el proceso nuevo las adopta; la primera actualización a esta versión sí las para | reiniciar kubelet o containerd no mata los pods | `virt-handler`: las VMs viven en pods independientes | `virt-daemon`: CH en pods | un orquestador por nodo gestiona los procesos de Firecracker |
| **Identidad del workload** | token OIDC corto acuñado en el servidor (`user_sub`, `act`) y **agente SSH del host por vsock**, sin que la clave entre en el guest | tokens de ServiceAccount proyectados | ídem | ídem | no se ha comparado |
| **Egress** | allowlist por tenant en un proxy + sumidero de DNS + nftables en el nodo, **activo por defecto** con el proxy; la IP de origen identifica la sandbox | NetworkPolicy (L3/L4); filtrado por nombre con proxies de terceros | NetworkPolicy, Multus | NetworkPolicy | según el despliegue |
| **Persistencia** | disco local retenido al parar, fijado a su nodo, con caducidad (7 días por defecto); reanudar es **arrancar de nuevo** sobre ese disco | `PersistentVolumeClaim` (puede cambiar de nodo); pausa y reanudación | PVC/CDI; migración en vivo | PVC | snapshots de memoria y disco en almacenamiento de objetos |
| **Arranque** | unos 5 s en frío y 3 s al reanudar, medidos en el laboratorio; sin pool precalentado | pools precalentados (`SandboxWarmPool`); el tiempo depende del runtime | VMs completas con libvirt y QEMU: más lento y más pesado por VM | la huella por VM que declara el proyecto es de ≈30 MB | restaura un snapshot (sin arrancar kernel) |
| **API y SDK** | REST y la CLI `asp`; sin SDK | CRDs, `kubectl`, SDK Go y Python | CRDs, `virtctl` | CRDs | REST y SDKs |
| **Un solo host** | `asp-server` (tres comandos) | k3s más manifiestos y `kata-deploy` | un cluster y un operador | un cluster, cert-manager y Kubernetes ≤ 1.25 | «Embed»: una máquina con KVM por Docker Compose |
| **Observabilidad y control de acceso** | `/metrics` de Prometheus, logs, eventos por sandbox; roles del IdP y API keys | métricas, RBAC, auditoría y eventos del cluster | ídem | ídem | observabilidad propia (ClickHouse) |
| **Madurez** | un laboratorio; sin releases (la primera será la 0.1.0) | `v1beta1`; SIG Apps; Kata y gVisor maduros | proyecto *incubating* de la CNCF, en camino de graduarse | «work in progress», API inestable | en producción en E2B |

## Lo que Kubernetes cubre y ASP reimplementa

Planificación por capacidad, inventario y ciclo de vida de nodos, `cordon`, detección de nodos caídos y fencing, actualizaciones sin matar las cargas, una PKI para los nodos, RBAC, una API con SDKs, métricas, pools precalentados y almacenamiento que cambia de nodo. En el código, el plano de control y el node-agent son la mayor parte del proyecto, y casi todo eso está ahí porque ADR-0004 decidió que **las sandboxes no son Pods** y entonces hay que hacerlo en casa. Es un coste permanente, que solo compensa si lo que se gana es mayor que lo que cuesta.

## Lo que ASP hace y Kubernetes no hace de serie

- **Credenciales fuera del guest.** El agente SSH del host se expone por vsock, filtrado (solo identidades y firmas, con confirmación opcional por sandbox), y el token OIDC lo acuña el plano de control con claims que el guest no elige. Un guest comprometido no se lleva una clave.
- **Egress por tenant y por nombre**, con el origen atribuido a la sandbox por IP y no por cabeceras, sin instalar un proxy de terceros.
- **La sesión como producto**: la CLI está pensada para que el shell de un harness de agentes (OpenCode en modo bash) apunte a `asp session exec`, con el directorio del proyecto compartido por virtiofs y el disco que sobrevive a parar.
- **Un solo host sin un cluster**: `asp-server` hace y mantiene la base de datos, las claves y el certificado.

Esas cosas son portables: podrían entregarse como un DaemonSet (el proxy de identidad y egress de cada nodo) y un controlador que mapee `Sandbox` a lo que ASP necesita. No hay planes de hacerlo hoy.

## Cuándo conviene cada camino

- **«Mi servidor, mi equipo, de uno a tres hosts.»** Un orquestador propio pequeño gana, pero solo si es de verdad pequeño: un binario, un comando, secretos generados, SQLite. Kubernetes ahí es demasiado. Es el caso para el que está `asp-server`.
- **«Corporativo, varios nodos, varios tenants, un IdP, actualizaciones sin cortes, auditoría.»** Kubernetes con `agent-sandbox` y Kata ya lo tiene y con años de ventaja. ASP aportaría la capa de identidad y de egress, y reimplementar el resto es un coste permanente. Si ese es tu caso, evalúa esa opción primero.
- **Si lo que quieres son arranques de decenas de milisegundos y miles de sandboxes cortas**, E2B (restauración de snapshots) o un pool precalentado están diseñados para eso; ASP no ([#144](https://github.com/luisgf/agent-sandbox-platform/issues/144) estudia si merece la pena).

## Qué se ha decidido y qué se está evaluando

[ADR-0004](../adr/0004-k8s-scope.md) decidió que Kubernetes es opcional y se limita al despliegue del plano de control, y descartó la sandbox-como-Pod con razones reales (dos planificadores, CNI frente a TAP, recuperación ambigua). Lo que **no** examinó es el proyecto `agent-sandbox`, ni Kata con Cloud Hypervisor como `RuntimeClass`, ni Virtink o E2B como alternativas completas. El ADR-0013 (reservado, [#143](https://github.com/luisgf/agent-sandbox-platform/issues/143)) los medirá con la misma prueba de punta a punta que usa ASP (sesión, parar, reanudar, workspace, identidad, egress) sobre un k3s, y decidirá con datos. Hasta entonces, la superficie de orquestación (planificador, fencing, PKI, liveness) no crece: se corrigen sus errores y la inversión va a los diferenciales y a la simplicidad de un host.

## Fuentes

Consultadas el 8 de octubre de 2026.

- Agent Sandbox: [visión general](https://agent-sandbox.sigs.k8s.io/docs/getting_started/overview/) (CRDs, runtimes gVisor y Kata, `v1beta1`, SDK de Go y Python, pausa y reanudación).
- Kata Containers: [arquitectura](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md) (shim v2 y `RuntimeClass`).
- KubeVirt: [guía de usuario](https://kubevirt.io/user-guide/) y su estado en la CNCF ([*incubating* desde 2022](https://www.cncf.io/blog/2022/04/19/kubevirt-becomes-a-cncf-incubating-project/); [camino a la graduación en 2026](https://siliconangle.com/2026/03/30/kubernetes-virtualization-approaches-cncf-graduation-kubeconeu/)).
- Virtink: [README](https://github.com/smartxworks/virtink) (Kubernetes v1.16–v1.25, cert-manager v1.0–v1.8, ≈30 MB por VM, API inestable).
- E2B: [infraestructura abierta](https://github.com/e2b-dev/infra) (Apache-2.0, Firecracker, restauración de snapshots, opciones de despliegue).
- gVisor: [documentación](https://gvisor.dev/docs/).
