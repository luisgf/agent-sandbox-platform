# Glosario

Las palabras de ASP, agrupadas por lo que nombran. Una definición dice qué es la cosa en el código de hoy y dónde se explica; no la historia (para eso, [la historia de las fases](../history.md)). Si una palabra de otra parte del proyecto no está aquí, es un fallo del glosario: abre una issue.

- [Piezas](#piezas)
- [Sandboxes y sesiones](#sandboxes-y-sesiones)
- [La red de una sandbox](#la-red-de-una-sandbox)
- [Identidad y acceso](#identidad-y-acceso)
- [Nodos y reparto](#nodos-y-reparto)
- [Confianza](#confianza)
- [Nombres de la documentación y de los commits](#nombres-de-la-documentación-y-de-los-commits)

## Piezas

- **ASP** (Agent Sandbox Platform): Aislamiento de agentes de IA en microVMs: una sesión larga por agente, con su workspace, su política de salida a la red y sus credenciales fuera del guest. [Arquitectura](../architecture.md).
- **plano de control** (*control plane*, `asp-control-plane`): La API HTTP, el planificador que coloca las sandboxes en nodos, el monitor que da por perdidos los nodos que callan, el almacén (memoria, SQLite o Postgres), la PKI de los nodos y el emisor de tokens OIDC de las sandboxes. [Referencia de la API](api.md).
- **node-agent** (`asp-node-agent`): El programa de cada servidor con KVM. Sondea el plano de control, arranca y para las microVMs de su nodo, les pone red (TAP, nftables, proxy de egress) y les da acceso al host por vsock. Uno por servidor. [Cómo ejecuta un nodo las sandboxes](../concepts/node-runtime.md).
- **nodo**: Un servidor Linux con KVM que corre un node-agent y aloja sandboxes. Se da de alta con un token de enroll ([instalar un nodo](../how-to/install-node.md)).
- **pod-daemon**: El demonio de dentro del guest que ejecuta los comandos que le llegan. Escrito en Rust, habla HTTP por vsock (puerto 26500) y es root **dentro** de la VM, a propósito: lanza cada comando como el dueño del workspace, como otro usuario o como root si se pide.
- **`asp`**: La línea de órdenes: sesiones, sandboxes, nodos, claves, imágenes, `asp doctor`. [Órdenes](cli.md).
- **`asp-server`**: Plano de control y nodo de un mismo host en un proceso, con SQLite y los secretos generados ([un solo host](../how-to/single-host.md)).
- **guest** (invitado): El sistema que corre dentro de la microVM: un kernel y una imagen Debian con el pod-daemon.
- **microVM**: Una máquina virtual KVM mínima, arrancada por un VMM sin BIOS ni dispositivos que sobren. Cada sandbox es una.
- **VMM** (monitor de máquinas virtuales): El proceso que emula una VM. ASP usa **Cloud Hypervisor** (CH); un perfil de Firecracker está en la [hoja de ruta](../roadmap.md). Cada VM tiene el suyo, como un usuario sin privilegios propio ([ADR-0015](../adr/0015-unprivileged-vmm.md)).
- **KVM**: El hipervisor del kernel de Linux (`/dev/kvm`). Sin él no hay aislamiento: solo se puede probar el plano de control.
- **FakeVMM** / **dry-run** (`--dry-run`): Un VMM de mentira que deja probar el plano de control y el node-agent sin KVM. Ejecuta los comandos en un proceso normal: **no aísla nada**. Lo que usan los tests y los smokes de CI.
- **imagen del guest**: El kernel (`vmlinux`) y el disco raíz (`rootfs.img`) con que arranca una sandbox. Se instalan con `asp image pull`, que los comprueba contra el `SHA256SUMS` de la release; el nodo vuelve a comprobarlos antes de arrancar ([la imagen del guest](../../images/guest/README.md)).

## Sandboxes y sesiones

- **sandbox**: Una microVM con su disco, su CPU y memoria, y las políticas de su tenant. Es lo que crea `POST /v1/sandboxes`; vive en un nodo.
- **estado** de una sandbox: `requested` (colocada en un nodo, a la espera de que la reclame), `starting`, `running`, `stopping`, `stopped` (parada, con su disco), `failed`, `deleting` y `deleted` (final; la fila queda para la auditoría). `scheduled` y `paused` existen en el contrato con los nodos, pero el plano de control no los usa. [Estados](api.md#convenciones).
- **sesión** (`asp session`): Cómo un agente usa una sandbox: una, durante horas o días, con muchos `exec`; no una sandbox por comando. El **puntero** de la sesión es un fichero local (`~/.cache/asp/sessions/<nombre>.json`) con el id de la sandbox y la URL del plano de control, sin secretos: otro equipo no lo ve y el plano de control no conoce el nombre. [ADR-0009](../adr/0009-agent-sessions.md), [sesiones](../ops-asp-session.md).
- **one-shot** (`asp sandbox run`): Crear, ejecutar un comando y destruir, en un proceso. Es la primitiva de CI y de operaciones, no la integración de un agente.
- **tenant**: Un identificador al que pertenecen sandboxes, claves de API y reglas de egress. Su fila se crea la primera vez que se usa. Un llamante atado a un tenant (una clave de tenant, un token del IdP) solo ve el suyo: la sandbox de otro responde 404.
- **exec**: Ejecutar un comando en una sandbox `running`. **Acumulado** (por defecto en la API): responde cuando el comando acaba y tiene límite de tiempo (`ASP_BUFFERED_EXEC_TIMEOUT`). **En streaming** (`?stream=1`, por defecto en `asp session exec`): una línea JSON por trozo de salida (NDJSON), sin límite de tiempo. Con **PTY** el guest da un pseudoterminal.
- **workspace**: Un directorio del host compartido con el guest por virtiofs y montado en `/workspace` (`--workspace`). Todo lo que haya en él es legible y escribible por el guest. [Modelo de seguridad](../concepts/security-model.md#el-workspace-del-host).
- **virtiofs** / **virtiofsd**: El sistema de ficheros compartido de virtio y el demonio del host que lo sirve. `virtiofsd` corre como root en un chroot del workspace: es el límite conocido de ADR-0015.
- **disco retenido**: El disco de una sandbox **parada**: un `stop` no lo borra, un `resume` arranca la sandbox otra vez sobre él en el mismo nodo, y caduca a los 7 días (`ASP_STOPPED_SANDBOX_TTL`) o por el tope por tenant. [ADR-0012](../adr/0012-retained-disks.md).
- **`resume`** / **`start`**: Reanudar una sandbox parada. En la API es `POST /v1/sandboxes/{id}/start`; `boot_count` sube en uno y `booted_at` dice si llegó a correr alguna vez (si no, no hay un disco que merezca conservarse).
- **reaper de inactividad** (*idle reaper*): El proceso del plano de control que **para** (sin borrar el disco) las sandboxes sin actividad. Apagado por defecto: se enciende con `ASP_SANDBOX_IDLE_TIMEOUT`. Un `exec` en curso cuenta como actividad.
- **`stop_reason`**: Por qué una sandbox paró o falló sin que nadie lo pidiera: `idle_timeout`, `node_lost`, `node_agent_restarted`, `vmm_exited`, `unscheduled`, `retention_expired` o `tenant_cap`.
- **harness**: El programa de un agente (OpenCode y similares) que decide qué herramientas ejecutar. Se engancha a ASP sustituyendo su shell por un wrapper que llama a `asp session exec` ([integración con OpenCode](../../integrations/opencode/README.md)).
- **auto-provision** (`ASP_AUTO_PROVISION=1`): El plano de control marca las sandboxes como `running` sin que ningún nodo las arranque. Solo para smokes sin nodos; engaña sobre lo que corre.

## La red de una sandbox

- **TAP**: La interfaz de red virtual de una sandbox en el host (`asp-<id corto>`), con su propia red /30 dentro de `--guest-subnet`. El host reconoce una sandbox por **su dirección de origen**, no por lo que el guest escriba. [Red y egress](../concepts/networking-and-egress.md).
- **egress**: Lo que el guest envía hacia fuera. Solo sale por el **proxy de egress** del nodo (HTTP(S) y DNS), que decide por nombre y puerto según la política del tenant.
- **política de egress**: Las reglas de un tenant: `host` o `*.dominio`, y un puerto (sin puerto, 80 y 443). Con alguna regla activa el modo es `deny-default`; sin ninguna es `allow-all` solo si el plano de control corre con `ASP_EGRESS_DEFAULT_ALLOW`, y si no, nada sale.
- **redirect de nftables**: Las reglas (tabla `asp_egress`) que obligan al tráfico HTTP(S) y DNS del guest a pasar por el proxy, aunque el guest se salte `HTTP_PROXY`.
- **`enforce`** / **`soft`** (`--nft-egress-mode`): Qué hace un nodo que no puede aplicar el redirect: `enforce` (el valor por defecto) se niega a arrancar; `soft` arranca sin forzar el egress. Un nodo declara `egress_enforced` al registrarse solo si lo aplica de verdad. **SoftFail** es el nombre antiguo de esa tolerancia, de cuando también valía para el TAP; hoy un TAP que no se puede crear hace fallar la sandbox.
- **vsock**: El canal guest↔host sin red. Tres puertos: **26500** (host→guest: el HTTP del pod-daemon), **26501** (guest→host: el agente SSH) y **26502** (guest→host: la identidad). Cada uno llega al servicio de **su** sandbox. Con Cloud Hypervisor es un vsock *híbrido*: sobre un socket unix por VM ([del guest al host](../concepts/node-runtime.md#del-guest-al-host-identidad-y-agente-ssh---host-vsock)).
- **`--local-net`** (red local bajo demanda): El opt-in de una sesión para que **todo** su tráfico salga por el portátil del usuario a través de un túnel WireGuard, no por el proxy del nodo. Todo o nada: sin CIDR ni excepciones. Si el túnel cae, el nodo corta el tráfico y no vuelve en silencio al proxy. [ADR-0010](../adr/0010-on-demand-local-net.md), [operación](../ops-local-net.md).
- **grant** (de `local-net`): El secreto de un solo uso (10 minutos) que el CLI presenta para levantar el túnel. Se guarda solo su hash.

## Identidad y acceso

- **IdP**: El proveedor de identidad OIDC de tu organización (Keycloak, Entra ID, Okta). Con él, el tenant, el sujeto y el rol de una persona salen de su token ([conectar un IdP](../how-to/idp.md)).
- **rol**: Lo que un token del IdP puede hacer: `admin` (todo en su tenant, y administrar nodos, claves y la política de egress), `operator` (listar y ver todo el tenant; actuar en lo ajeno solo con una concesión), `user` (crear y actuar en lo suyo), `viewer` (solo leer). Salen de los grupos del token (`ASP_IDP_ROLE_*`).
- **`exec-any`** / **`destroy-any`**: Las concesiones que dejan a un `operator` ejecutar o parar/borrar sandboxes que no son suyas. Salen de un grupo del token (`ASP_IDP_EXEC_ANY_GROUP`, `ASP_IDP_DESTROY_ANY_GROUP`).
- **`owner_sub`** / **`actor_sub`**: El dueño de una sandbox (el sujeto del token con que se creó) y quien hace cada acción (el sujeto del token, `apikey:<prefijo>`, `node:<id>`). Salen siempre de la credencial, nunca del cuerpo de la petición, salvo en el laboratorio abierto.
- **clave de API**: Un secreto `asp_…` que se guarda con hash. De **tenant** (solo actúa en el suyo) o de **plataforma** (cruza tenants, administra nodos y claves, y puede llamar a las rutas de nodo). Se gestiona con `asp apikey`.
- **token de arranque** (*bootstrap*) / **token de enroll**: Con qué se da de alta un nodo. El de arranque (`ASP_NODE_BOOTSTRAP_TOKEN`) lo comparten todos los nodos y solo enrola ids nuevos o revocados; el de enroll es de un solo uso, caduca y puede fijarse a un nodo (`asp node enroll-token`).
- **tokens de workload** / emisor OIDC del plano de control: Los JWT de corta vida que el plano de control firma para una sandbox (`sub`, `tenant_id`, `sandbox_id`, `user_sub`, `act` salen del almacén; el guest solo elige `aud`) para que llame a servicios sin llevar claves largas. Su JWKS está en `/oidc/jwks.json`.
- **agente SSH del host** (puente SSH): El servicio del host que deja al guest **firmar** con las claves SSH del dueño sin entregárselas: solo reenvía listar y firmar. Con `--ssh-agent-confirm`, cada firma necesita una aprobación de esa sandbox; con `ASP_SSH_AGENT_SOCK_TEMPLATE`, cada sandbox usa el socket de su dueño.
- **laboratorio abierto** (`ASP_INSECURE_OPEN_API=1`): El único modo en que el plano de control acepta peticiones sin credencial. Para smokes y un portátil; avisa al arrancar. Una petición con credencial se comprueba igual.

## Nodos y reparto

- **enroll** (alta de un nodo): El nodo presenta un token y recibe un certificado de cliente para mTLS, con su clave y la CA. [Añadir un servidor](../ops-multi-node.md#añadir-un-servidor).
- **mTLS**: TLS en los dos sentidos. Los nodos hablan con el plano de control con su certificado (cuyo CN es su id: solo pueden actuar por sí mismos), y el plano de control llama a los agentes con el suyo.
- **CA de enrollment** / **`--control-plane-ca`**: Las **dos raíces de confianza**: la primera (`ASP_CA_CERT`/`ASP_CA_KEY`) firma los certificados de los nodos y el de cliente del plano de control; la segunda, que cada nodo guarda, solo valida el certificado TLS del plano de control. Mezclarlas es el error clásico ([las dos raíces](../ops-multi-node.md#las-dos-raíces-de-confianza)).
- **`rotate-cert`** / **`revoke`**: Renovar el certificado de un nodo (el propio nodo lo hace solo, con un tercio de su vida por delante) y retirarlo para siempre (no vuelve con un heartbeat; necesita re-enrolarse).
- **colocación** (*placement*): Elegir en qué nodo va una sandbox al crearla: el nodo tiene que estar vivo, no revocado, sin cordon, aceptar trabajo, tener el perfil de VMM y espacio de CPU, memoria y sandboxes. Si no hay ninguno, `503` con los motivos y `Retry-After`.
- **`spread`** / **`binpack`** (`ASP_SCHED_POLICY`): Repartir las sandboxes entre los nodos (el menos cargado primero, el valor por defecto) o llenar uno antes de pasar al siguiente.
- **sobresuscripción** (*overcommit*) de CPU: El planificador cuenta cada núcleo del nodo como 4 (`ASP_SCHED_CPU_OVERCOMMIT`). La memoria **nunca** se sobresuscribe, y cada VM cuenta además un margen para el hipervisor.
- **cordon** / **uncordon**: Sacar un nodo del reparto sin tocar lo que corre, y volver. No existe `drain`: las sandboxes no se mueven; se espera a que acaben o se paran. [Mantenimiento](../ops-multi-node.md#mantenimiento).
- **sondeo de trabajo** (`GET /v1/nodes/{id}/work`): Cómo se entera un nodo de lo que tiene que hacer, cada 2 s. Devuelve las sandboxes que necesitan su acción, `assigned` (todas las que debe tener corriendo: **el nodo detiene cualquier VM que no figure en ella**), `retained` (las paradas cuyo disco guarda) y la política de egress de cada tenant. Cuenta también como señal de vida.
- **claim**: El nodo asignado reclama una sandbox `requested`, que pasa a `starting`. Solo puede el nodo en que se colocó.
- **reconciler**: El bucle del node-agent que lleva lo que corre en el host a lo que dice el sondeo: arranca, para, reaplica políticas y borra lo que nadie posee (incluidos los discos que ninguna sandbox reclama).
- **monitor de nodos**: El proceso del plano de control que mira cuándo habló cada nodo por última vez: a los 90 s (`ASP_NODE_STALE_AFTER`) deja de recibir sandboxes y a los 5 min (`ASP_NODE_FAILOVER_AFTER`) se le hace fencing y sus sandboxes pasan a `failed` con `node_lost`.
- **fencing** (*STONITH*) / **FenceProvider**: Apagar un nodo perdido antes de dar por muertas sus sandboxes, para que una partición de red no deje dos copias. El *provider* es un webhook (funciona), Redfish o IPMI (stubs). **Autodefensa** del nodo (*self-fence*): un nodo para las VMs que ya no le asigna el plano de control; no es STONITH.
- **confinamiento** (`--vm-confine`) y **adopción**: Con systemd, cada VMM y cada `virtiofsd` corren en un servicio transitorio propio (`asp-vm-<id>`, en `asp-vms.slice`) que no depende del agente. Por eso reiniciar o actualizar el agente **no para** las VMs: el proceso nuevo las **adopta** a partir de un registro por VM. [ADR-0014](../adr/0014-vms-outlive-the-agent.md).

## Confianza

- **frontera de confianza**: Un límite entre algo que ASP protege y algo que no controla. El [modelo de seguridad](../concepts/security-model.md) lista cada frontera y qué garantiza y qué no.
- **atestación** (*attestation*): Una declaración que el nodo **firma** al arrancar cada sandbox: los SHA-256 del kernel y de la imagen base, la versión del hipervisor y si es un arranque nuevo o una reanudación. Es una firma de software, no TPM ni SEV: prueba que el nodo dice haber arrancado esa imagen. **medido** (*measured*): la declaración trae digests reales; **permitida** (*allowlisted*): están en `ASP_ATTEST_ALLOWED_IMAGES`.
- **lista de imágenes permitidas**: El fichero que dice qué pares kernel + imagen puede declarar un nodo; `node-agent --print-measurement` imprime la entrada de un nodo.

## Nombres de la documentación y de los commits

- **ADR** (*architecture decision record*): Una decisión de diseño con su contexto, sus alternativas y sus consecuencias, en `docs/adr/`. Cada uno lleva un estado ([índice](../adr/README.md)).
- **códigos de fase** (`1a` … `3m`): Cómo se llamó a cada tramo del MVP en commits, tests y ADRs antiguos: `2d` = hardening operativo (rotación de certificados, mTLS estricto, confirmación de firmas SSH); `2e` = nftables completo y agente SSH automático en el guest; `3l` = red local bajo demanda; `3m` = varios servidores; `3n` = atribución de flujos de red; `3u` = identidad multiusuario. Ya no se usan para nombrar nada nuevo: [la historia de las fases](../history.md).
- **laboratorio** (*lab*): Dos cosas. El **laboratorio abierto** es un modo del plano de control (arriba). **El laboratorio** a secas es el host de pruebas de los mantenedores, donde se comprueba con VMs de verdad ([lab/](../lab/README.md)); nada del producto depende de él.
