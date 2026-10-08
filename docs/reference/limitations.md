# Límites conocidos

Lo que ASP **no hace** hoy, junto en una página: antes estaba repartido entre el README, la hoja de ruta y los «límites honestos» de varios ADRs. ASP es un MVP (versión 0.x) endurecido por fases; cada fila dice qué pasa, qué hacer y dónde se sigue. Los límites de seguridad (qué frontera protege qué y qué no) están en el [modelo de seguridad](../concepts/security-model.md#lo-que-asp-no-cubre) y aquí solo se resumen; **qué cuenta como vulnerabilidad**, en [SECURITY.md](../../SECURITY.md). Lo que está previsto hacer: [hoja de ruta](../roadmap.md).

- [Dónde corre](#dónde-corre)
- [Qué se ha probado y qué no](#qué-se-ha-probado-y-qué-no)
- [Nodos y reparto](#nodos-y-reparto)
- [Sesiones, discos y retención](#sesiones-discos-y-retención)
- [Red y egress](#red-y-egress)
- [Identidad y acceso](#identidad-y-acceso)
- [La API y la línea de órdenes](#la-api-y-la-línea-de-órdenes)
- [Operación](#operación)

## Dónde corre

| Límite | Qué pasa | Más |
|---|---|---|
| **Los nodos son Linux con KVM** | Sin `/dev/kvm` no hay aislamiento. `--dry-run` (`FakeVMM`) deja probar el plano de control y el node-agent, pero ejecuta los comandos en un proceso normal: no aísla nada. Virtualización anidada: solo en un laboratorio, con peor densidad y latencia | [Instalar un nodo](../how-to/install-node.md) |
| **Solo guests Linux** | No hay guests Windows ni de otro sistema, y no se soportan cargas que necesiten `NET_ADMIN` en el guest | [Hoja de ruta, fuera de alcance](../roadmap.md#fuera-de-alcance) |
| **Un VMM: Cloud Hypervisor** | Con la versión que valida quien opera (`vmm_profile` es `cloud-hypervisor`). No hay perfil de Firecracker | [ADR-0001](../adr/0001-vmm-choice.md) |
| **x86-64 probado; arm64, solo compilado** | Los binarios se construyen también para arm64 y el pod-daemon se comprueba para `aarch64`, pero ningún nodo se ha ejecutado en arm64 | [Publicar una versión](../how-to/release.md) |
| **El confinamiento de las VMs pide systemd y root** | Sin él (`--vm-confine=off`, un host sin systemd, `--ch-api-socket`) el VMM es hijo del agente: sigue como root y reiniciar el agente **detiene** sus VMs | [ADR-0014](../adr/0014-vms-outlive-the-agent.md), [ADR-0015](../adr/0015-unprivileged-vmm.md) |
| **El cliente `asp` corre en Linux y macOS; el nodo, solo en Linux** | `--local-net` desde macOS no está verificado de punta a punta | [Red local bajo demanda](../ops-local-net.md) |
| **Kubernetes solo despliega el plano de control** | Las sandboxes no son Pods; los nodos necesitan KVM y no van en contenedores. Hay imagen del plano de control | [ADR-0004](../adr/0004-k8s-scope.md), [ASP y Kubernetes](../concepts/asp-vs-kubernetes.md) |

## Qué se ha probado y qué no

Lo que dice «funciona» en el resto de la documentación tiene detrás una de estas tres cosas, y conviene saber cuál.

| Qué | Cómo se comprueba |
|---|---|
| El plano de control, el planificador, la API, el CLI y el node-agent con `FakeVMM` | Tests en cada commit (CI): Go con `-race`, `staticcheck`, los tres almacenes (memoria y SQLite siempre; Postgres en los tests del almacén), los smokes con dos nodos *dry-run*, y Rust para el pod-daemon |
| El aislamiento de verdad: arrancar, ejecutar, workspace, parar, reanudar, borrar con KVM; el `enforce` de nftables a prueba de salto con un guest de verdad; el usuario sin privilegios por VM; `virtiofs`; `--local-net` | **A mano, en un host de pruebas** (`make e2e-kvm`, `smoke-egress-kvm`, `smoke-vmm-user-kvm`; [comprobar un nodo](../how-to/e2e-kvm.md)). **CI no tiene KVM**: un cambio que rompa esto pasa los tests. (Las reglas de la tabla de nftables sí se cargan y prueban en CI, en namespaces y sin guest: `smoke-egress-nft.sh`.) Un carril nocturno con KVM está pendiente (#130) |
| Varios servidores | **No se ha probado con un laboratorio de varios servidores KVM.** El reparto, el *failover* y el fencing se prueban con nodos *dry-run* y con un host real |

Además: la integración con IdPs se ha probado con **Keycloak**, no con Entra ID ni con Okta; y de `--local-net` hay comandos `ip`/`wg` reales probados a mano con dos sesiones y el cliente en un *network namespace*, pero no el reenvío a una LAN real, el DNS de casa ni el cliente macOS.

## Nodos y reparto

| Límite | Qué pasa | Más |
|---|---|---|
| **No hay migración** | El disco de una sandbox vive en el servidor donde se creó. Si el servidor se pierde, sus sesiones se pierden con él (`failed`, `node_lost`; abre otra con `asp session start --force`). Una sandbox parada queda fijada a su nodo: `resume` va a ese nodo (503 si está lleno, 409 si está en cordon, caído o revocado) | [ADR-0011](../adr/0011-multi-node.md), [ADR-0012](../adr/0012-retained-disks.md) |
| **No se puede cambiar el tamaño al reanudar** | Una sandbox reanudada tiene la CPU y la memoria con que se creó, y el disco que dejó | [ADR-0012](../adr/0012-retained-disks.md) |
| **La colocación no conoce `--workspace`** | La ruta del host tiene que existir en el nodo que el planificador elija: ponla en todos | [ADR-0011](../adr/0011-multi-node.md) |
| **Un node-agent por servidor** | Al arrancar borra todos los TAP `asp-*` y los túneles `wg-asp-*` del host, sean suyos o no | [Cómo ejecuta un nodo las sandboxes](../concepts/node-runtime.md) |
| **La CPU se sobresuscribe; la memoria, nunca** | Cada núcleo cuenta 4 (`ASP_SCHED_CPU_OVERCOMMIT`) y cada VM cuenta un margen de memoria para el hipervisor. Sin hueco, `503` con los motivos | [Varios servidores](../ops-multi-node.md#capacidad) |
| **Un solo plano de control** | Con más de una réplica sobre el mismo Postgres, cada una corre su monitor de nodos: el CAS lo hace seguro, pero el fencing podría repetirse, y cada réplica se emite su propio certificado de cliente para llamar a los agentes. No se ha probado con réplicas | [ADR-0011](../adr/0011-multi-node.md) |
| **Un nodo que calla tarda en darse por perdido** | A los 90 s deja de recibir sandboxes y a los 5 min se declara perdido (`ASP_NODE_STALE_AFTER`, `ASP_NODE_FAILOVER_AFTER`). Entre medias sus sandboxes siguen ahí | [Nodos caídos](../ops-multi-node.md#nodos-caídos) |
| **El fencing real no está validado** | El *provider* que funciona es un webhook; Redfish e IPMI son stubs. La autodefensa del nodo (para toda VM que ya no le asignan) no es STONITH: un nodo particionado sigue corriendo sus VMs hasta que vuelve o hasta el fencing | [Fencing](../how-to/security-operations.md#fencing-stonith) |
| **Reiniciar el servidor (no el agente) para sus VMs** | Las sandboxes pasan a `stopped` con `node_agent_restarted`, con su disco. Reiniciar o actualizar solo el agente no las toca | [Actualizar](../how-to/upgrade.md) |

## Sesiones, discos y retención

| Límite | Qué pasa | Más |
|---|---|---|
| **Los nombres de sesión son locales** | `~/.cache/asp/sessions/<nombre>.json` es del `$HOME` del cliente: otro equipo no lo ve y el plano de control no conoce el nombre. `asp session rm --local` solo borra el fichero | [ADR-0009](../adr/0009-agent-sessions.md), [sesiones](../ops-asp-session.md) |
| **El reaper de inactividad está apagado** | Sin `ASP_SANDBOX_IDLE_TIMEOUT`, una sesión sin `stop` vive hasta que alguien la pare o la borre. No hay un tiempo distinto por sesión | [Sesiones](../ops-asp-session.md) |
| **Una sesión comparte estado entre sus `exec`** | Un comando sucio deja procesos y ficheros al siguiente: el precio de tener un workspace | [Modelo de seguridad](../concepts/security-model.md#el-guest-frente-al-host-y-frente-a-otras-sandboxes) |
| **El PTY no es un terminal completo** | stderr se mezcla en el pseudoterminal; el fin de entrada son dos Ctrl-D (no un EOF); el CLI fija el tamaño al empezar y no manda los cambios de ventana (la API sí admite un `rows`/`cols` posterior); los bytes viajan como cadenas JSON. Para un pipe de verdad, `--no-pty` | [Sesiones](../ops-asp-session.md#virtiofs-y-pty--qué-aterrizó) |
| **Las paradas caducan** | A los 7 días (`ASP_STOPPED_SANDBOX_TTL`) o por el tope por tenant (`ASP_MAX_STOPPED_PER_TENANT`, sin tope por defecto): su disco se borra | [ADR-0012](../adr/0012-retained-disks.md) |
| **Un disco retenido no es una copia de seguridad** | Un corte de luz deja el ext4 con el diario por reproducir (se pueden perder las últimas escrituras); borrar es un `unlink`, sin borrado seguro; ASP no cifra los discos (eso lo da el disco del host) | [ADR-0012](../adr/0012-retained-disks.md), [copias](../how-to/backup-and-restore.md#los-discos-y-los-workspaces) |
| **Las imágenes viejas montan el workspace a mano** | Una imagen anterior a `workspace-virtiofs.service` no monta `/workspace` sola: `mount -t virtiofs workspace /workspace` o reconstruirla. Sin el binario `virtiofsd` en el nodo, el arranque con `--workspace` falla | [Sesiones](../ops-asp-session.md) |
| **`virtiofsd` corre como root** | Es el límite conocido de que el VMM corra sin privilegios; quitárselo exige traducir ids o un namespace de usuario por VM | [ADR-0015](../adr/0015-unprivileged-vmm.md) |
| **Todo lo que hay en un workspace lo lee y lo escribe el guest** | Comparte solo un directorio que el guest pueda ver entero | [Modelo de seguridad](../concepts/security-model.md#el-workspace-del-host) |

## Red y egress

| Límite | Qué pasa | Más |
|---|---|---|
| **El egress filtrado es el que aplica `enforce`** | Con el proxy y el redirect de nftables en `enforce` (el valor por defecto), el guest solo sale por HTTP(S) y DNS hacia lo que su tenant permite. En `soft` el proxy es voluntario. Las reglas de la tabla se prueban en CI en namespaces (`smoke-egress-nft.sh`), pero **que un guest de verdad no las salte solo se comprueba con KVM y TAP reales** | [Red y egress](../concepts/networking-and-egress.md) |
| **No cubre IPv6, QUIC ni UDP arbitrario** | Solo los puertos HTTP(S) configurados y el DNS | [ADR-0002](../adr/0002-networking.md) |
| **El proxy decide por nombre y puerto** | No por contenido: el MITM de `CONNECT` está apagado | [Modelo de seguridad](../concepts/security-model.md#el-guest-frente-a-la-red) |
| **`--local-net` es todo o nada** | El túnel saca **todo** el tráfico de la sesión por el portátil (y la política de egress del nodo no se aplica mientras está arriba). Sin CIDR, sin excepciones, sin servicios entrantes desde la LAN, sin compartir túnel entre sesiones. Si cae, el nodo corta el tráfico; no vuelve al proxy | [ADR-0010](../adr/0010-on-demand-local-net.md), [operación](../ops-local-net.md) |
| **Los flujos de red no se atribuyen a `owner_sub`** | El audit del proxy lleva el `sandbox_id` (la sandbox se reconoce por su IP de origen), pero no el dueño humano. Está diseñado y a medias | [ADR-0008](../adr/0008-network-flow-attribution.md) |
| **Un nodo sin `enforce` lo dice** | `egress_enforced` es `false` en `asp node list` y el plano de control avisa al registrarse: la política de un tenant no lo obliga | [Red y egress](../concepts/networking-and-egress.md) |

## Identidad y acceso

| Límite | Qué pasa | Más |
|---|---|---|
| **El tenant y el rol salen del token** | No hay tabla de pertenencia a tenants (`tenant_memberships`). Un token sin tenant es 401 salvo que el plano de control tenga `ASP_IDP_DEFAULT_TENANT` | [Conectar un IdP](../how-to/idp.md) |
| **Una clave de API no es una persona** | Es un principal de servicio: no puede nombrar un dueño y el audit la atribuye como `apikey:<prefijo>` | [Referencia de la API](api.md) |
| **El agente SSH no es seguro para varios usuarios sin plantilla** | Sin `ASP_SSH_AGENT_SOCK_TEMPLATE`, el puente global es el comportamiento antiguo. Con la plantilla y la confirmación, el aislamiento es por ruta y ASP no lanza los `ssh-agent`: crear el socket de cada usuario es cosa de quien opera | [Operaciones de seguridad](../how-to/security-operations.md#confirmación-del-agente-ssh) |
| **El «laboratorio abierto» no autentica** | `ASP_INSECURE_OPEN_API=1` acepta peticiones sin credencial. Es para smokes y un portátil, y avisa al arrancar | [Modelo de seguridad](../concepts/security-model.md#los-clientes-frente-al-plano-de-control) |
| **La atestación es de software** | Una firma del propio nodo, no TPM ni SEV: prueba que el nodo *dice* haber arrancado esa imagen. Un nodo comprometido puede declarar lo que quiera | [Modelo de seguridad](../concepts/security-model.md#atestación-fencing-y-nodos-perdidos) |
| **La CA de enrollment es la raíz de los nodos** | Quien tenga `ASP_CA_KEY` emite un nodo; y un nodo comprometido tiene root en su host (ve los discos y las claves de sus sandboxes) | [Modelo de seguridad](../concepts/security-model.md#los-nodos-y-el-plano-de-control) |

## La API y la línea de órdenes

| Límite | Qué pasa | Más |
|---|---|---|
| **La API puede cambiar entre versiones menores** | Mientras la versión mayor sea 0, el CHANGELOG dice lo que rompe. Está descrita en un documento OpenAPI que el propio plano de control sirve; no hay SDKs | [Referencia de la API](api.md) |
| **Solo un `exec` acumulado tiene límite de tiempo** | `ASP_BUFFERED_EXEC_TIMEOUT` (10 min por defecto, también en el guest); pasado, `504`. Un stream o un PTY dura lo que el comando | [Sesiones](../ops-asp-session.md) |
| **La integración con un agente es un wrapper de shell** | `integrations/opencode/` añade un plugin, instrucciones y configuraciones de ejemplo para OpenCode, que depende de un *hook* experimental (`experimental.chat.system.transform`): fija la versión de OpenCode | [Integración con OpenCode](../../integrations/opencode/README.md) |
| **`asp` no es un SDK ni una TUI** | Es una línea de órdenes; `asp sandbox run` espera con JSON acumulado | [Órdenes](cli.md) |

## Operación

| Límite | Qué pasa | Más |
|---|---|---|
| **Las migraciones solo van hacia delante** | No hay migración inversa: volver atrás tras una migración es restaurar la copia | [Actualizar](../how-to/upgrade.md#volver-atrás) |
| **Las copias de seguridad son un procedimiento, no una función** | Nada las hace solo, y una base restaurada más vieja que los nodos destruye lo que ellos tienen de más | [Copias y restauración](../how-to/backup-and-restore.md) |
| **Las métricas no traen SLOs ni alertas** | Hay métricas de Prometheus del plano de control y del agente, y un log por paso del ciclo de vida; no hay paneles ni reglas de alerta, ni métricas de revocaciones, firmas SSH denegadas o fallos del redirect de nftables | [Monitorizar](../how-to/monitoring.md) |
| **No hay releases todavía** | La primera será la 0.1.0. Hasta entonces, lo que hay es `main`; el flujo de publicación está escrito y falta activarlo | [Hoja de ruta](../roadmap.md#para-publicar-la-primera-versión-010) |
