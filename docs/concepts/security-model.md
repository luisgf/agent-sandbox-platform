# Modelo de seguridad

Qué protege ASP, de quién, con qué frontera, y qué **no** garantiza. Es la página que hay que leer antes de decidir si ASP sirve para una carga: la arquitectura por capas está en [architecture.md](../architecture.md), el diseño de cada pieza en los ADR, y cómo se reporta una vulnerabilidad, en [SECURITY.md](../../SECURITY.md).

## Qué se protege y de quién

El adversario de diseño es el **código que se ejecuta en una sandbox**: no es de confianza, puede estar escrito por un modelo que alguien ha manipulado, y se supone hostil. Todo lo demás se define por lo que ese código no debe poder hacer.

| Activo | Amenaza | Qué lo defiende |
|---|---|---|
| El host del nodo y las otras sandboxes | Escape del guest; movimiento lateral | Una microVM de Cloud Hypervisor por sandbox, con su TAP y su /30; el VMM como usuario sin privilegios, sin capabilities, con seccomp y en una unit que no abre sockets IP ([ADR-0001](../adr/0001-vmm-choice.md), [ADR-0015](../adr/0015-unprivileged-vmm.md)) |
| Las credenciales del operador (claves SSH, tokens) | Exfiltración desde el guest | Las claves nunca entran en el guest: el agente SSH del host se expone por vsock, filtrado, y los tokens OIDC los acuña el plano de control con claims que el guest no elige ([ADR-0003](../adr/0003-identity.md)) |
| La red del nodo, la LAN del nodo y los servicios del host | El guest llega a donde no debe (el API del agente, los metadatos de la nube, otra sandbox) | Deny-by-default: proxy, sumidero de DNS y nftables en el host, y destinos que el proxy nunca marca ([ADR-0002](../adr/0002-networking.md); [red y egress](networking-and-egress.md)) |
| El sistema de ficheros del nodo | Un tenant que pide compartir `/` por virtiofs | El workspace tiene que estar bajo `<raíz>/<tenant>/` del nodo, con los enlaces resueltos (`--workspace-root`) |
| Los datos de un tenant | Otro tenant, u otra persona del mismo tenant | Cada petición queda confinada al tenant de su credencial; los roles del IdP; el dueño de cada sandbox ([ADR-0007](../adr/0007-multi-user-identity.md)) |
| La identidad de los nodos | Un nodo falso, o un certificado robado | mTLS con un certificado por nodo, atado a su id; tokens de enroll de un solo uso; rotación y revocación ([ADR-0005](../adr/0005-fase-2d-hardening.md), [ADR-0011](../adr/0011-multi-node.md)) |
| El canal plano de control → nodo | `exec` sin autenticar por la red | En el mismo host, loopback con un token en un fichero que solo leen root y el plano de control; en otro host, mTLS donde el nodo solo acepta el certificado del plano de control |
| El plano de control | API anónima, una ruta mal cableada | Autenticación siempre activa (API keys o IdP), mTLS estricto opcional, rutas públicas mínimas |
| Dos nodos con la misma sandbox | Partición de red; *split-brain* | Solo el nodo asignado la reclama; transiciones validadas; monitor de nodos y fencing; el nodo para las VMs que ya no están en su conjunto `assigned` |

## Las fronteras de confianza

```text
┌─ Cliente ─────────────────────────────────────────────┐
│  Una API key o un token del IdP. No habla con el VMM. │
└───────────────────────────┬───────────────────────────┘
                            │ HTTPS
┌─ Plano de control ────────┴───────────────────────────┐
│  Autoridad de tenants y roles, claves y CA, JWKS,     │
│  verificación de atestaciones, fencing.               │
└───────────────────────────┬───────────────────────────┘
                            │ mTLS en los dos sentidos
                            │ (certificado del nodo ↔ del plano de control)
┌─ Nodo (privilegiado) ─────┴───────────────────────────┐
│  El único que habla con el VMM. Proxy de egress, DNS, │
│  nftables, host-vsock. Confianza: el host entero.     │
└───────────────────────────┬───────────────────────────┘
                            │ virtio / vsock (el guest no es de confianza)
┌─ Guest ───────────────────┴───────────────────────────┐
│  pod-daemon (root en la VM) y la carga. Sin NET_ADMIN, │
│  sin secretos duraderos. Solo pide `aud` y firmas.     │
└───────────────────────────────────────────────────────┘
```

Lo que está dentro de la frontera de confianza **tiene que ser de confianza**: el plano de control (si lo comprometen, todo lo está: emite tokens, certificados y ve los tenants) y el nodo (tiene root en el host, es el único que habla con el VMM, y sus claves valen para su propio id). La frontera de ASP es la **microVM**; los contenedores dentro del guest, si los hay, son comodidad.

## Qué garantiza cada frontera, y qué no

### El guest frente al host y frente a otras sandboxes

**Garantiza:** que el guest solo toca el host por tres caminos: el multiplexor de vsock (el host→guest, el 26500 del `pod-daemon`; el guest→host, el 26501 del agente SSH y el 26502 de la identidad, cada uno **ligado a su sandbox** porque el VMM conecta al socket de esa sandbox), el TAP y los dispositivos virtio. El VMM, que es el proceso más expuesto del nodo (emula discos, red, vsock y virtio-fs con lo que el guest escribe en las colas), corre como un usuario sin privilegios propio por VM, sin capabilities y con seccomp; no ve los ficheros de otra VM.

**No garantiza:** nada frente a un fallo de Cloud Hypervisor, de KVM o del kernel del host (es el límite de cualquier microVM; se reportan a su proyecto), ni frente a canales laterales entre VMs que comparten CPU. `virtiofsd` **sigue corriendo como root**, en `--sandbox chroot` sobre el workspace, porque tiene que actuar como el dueño de cada fichero: es el límite conocido de [ADR-0015](../adr/0015-unprivileged-vmm.md). El `pod-daemon` es root **dentro** de la VM, a propósito (lanza cada comando como el dueño del workspace, como otro usuario o como root si se pide): lo que aísla es la VM, no el usuario de ese proceso. Una sesión comparte estado entre sus `exec` (un comando sucio deja procesos y ficheros para el siguiente); es el riesgo que se compra a cambio de un workspace ([ADR-0009](../adr/0009-agent-sessions.md)).

### El guest frente a la red

**Garantiza:** que, con el proxy y el redirect de nftables en modo `enforce` (el valor por defecto con `--egress-proxy-listen`), el guest solo sale por HTTP(S) y DNS hacia lo que la política de su tenant permite, que cada sandbox se reconoce por **su IP de origen** y no por cabeceras que escribe el guest, y que un guest no alcanza el API local del agente, los metadatos de la nube, la LAN del nodo ni a otro guest, y que el proxy y el sink de DNS solo contestan a los guests y al propio nodo por loopback (sus listeners están en todas las direcciones: lo que lo impide es la tabla de nftables): el proxy comprueba la dirección **tras resolver el nombre** y nunca marca loopback, link-local, multicast, reservadas, las direcciones del nodo ni la red de los guests.

**No garantiza:** el egress es el que se aplica. En modo `soft`, o con el redirect apagado, el proxy es voluntario. No cubre IPv6 ni QUIC/UDP arbitrario: solo los puertos HTTP(S) configurados y el DNS. El proxy decide por nombre y puerto, no por contenido (el MITM de `CONNECT` está apagado). La integración continua usa `--dry-run`, así que **el bypass-proof de nftables solo se comprueba con KVM y TAP reales** (`make smoke-egress-kvm`). Con `--local-net`, una sesión que lo pidió saca **todo** su tráfico por el portátil del usuario, sin filtro de puertos ([ADR-0010](../adr/0010-on-demand-local-net.md)).

### El guest y las credenciales

**Garantiza:** que ninguna clave larga entra en el guest. El agente SSH del host solo reenvía listar identidades y firmar (añadir, borrar o bloquear claves da `SSH_AGENT_FAILURE`); con `--ssh-agent-confirm`, cada firma necesita una aprobación de **esa** sandbox, de un solo uso y con TTL; con `ASP_SSH_AGENT_SOCK_TEMPLATE`, cada sandbox usa el socket de su dueño. Los tokens OIDC de workload los firma el plano de control con `sub`, `tenant_id`, `sandbox_id`, `user_sub` y `act` que salen del store; el guest solo elige `aud`, y el token de una sandbox no se puede pedir desde la conexión de otra ([ADR-0003](../adr/0003-identity.md), [ADR-0007](../adr/0007-multi-user-identity.md)).

**No garantiza:** que un guest comprometido no **use** lo que se le deja usar: si la sesión tiene acceso al agente SSH, un guest hostil puede pedir firmas (por eso existe la confirmación, que es opcional y por sandbox, no por clave). Un token de workload vale lo que su TTL y su audiencia.

### El workspace del host

`asp session start --workspace /ruta` comparte un directorio del host con el guest por virtiofs. **Todo lo que haya en él es legible y escribible por el guest.** El nodo solo acepta rutas bajo `<raíz>/<tenant>/` de `--workspace-root` (`/srv/asp/workspaces` por defecto), con los enlaces simbólicos resueltos, y el plano de control las rechaza con 400 al crear si `ASP_WORKSPACE_ROOTS` está puesta. Sin esa regla, cualquiera que pudiera crear una sandbox podría pedir que el nodo exportara `/`, con los discos y las claves del nodo. **Comparte solo un directorio que el guest pueda ver entero.**

### Los clientes frente al plano de control

**Garantiza:** la autenticación está siempre activa; no hay un modo que falle abierto. Las rutas de usuario piden una API key (de un **tenant** o de **plataforma**, que cruza tenants y administra nodos) o, con `ASP_IDP_REQUIRED=1`, un token del IdP con audiencia; el dueño de cada sandbox (`owner_sub`) sale del token, no del cuerpo de la petición, y un cuerpo con otro dueño da 403. El rol (`admin`, `operator`, `user`, `viewer`) decide qué puede hacer cada persona sobre las sandboxes ([conectar un IdP](../how-to/idp.md)); las claves de API se guardan con hash y se rotan o revocan con `asp apikey`. Los nodos se administran solo con un administrador del IdP o una clave de plataforma, nunca con una de tenant.

**No garantiza:** un laboratorio abierto (`ASP_INSECURE_OPEN_API=1`) acepta peticiones sin credencial; está para smokes y avisa al arrancar. No hay pertenencia por tenant en SQL: el rol y el tenant salen del token. El plano de control sin TLS (HTTP en loopback) es un laboratorio; para varios hosts hace falta TLS, y mTLS estricto si no quieres peers anónimos en el listener.

### Los nodos y el plano de control

**Garantiza:** cada nodo tiene su certificado, válido para su id y para nada más (no para el endpoint que cite, ni para el nombre del plano de control); un id de nodo no puede ser una IP ni un nombre del certificado del plano de control; con `ASP_CLIENT_CA`, cada ruta de nodo compara el CN del certificado con el nodo para el que actúa; el alta de un nodo vivo exige un token de enroll de un solo uso fijado a él (el token de arranque compartido solo enrola ids nuevos o revocados); un certificado se rota con el vigente o con un administrador, y se revoca.

La PKI tiene **dos raíces de confianza**, y mezclarlas es el error clásico:

| Pregunta | Raíz | Qué firma |
|---|---|---|
| ¿Este nodo es quien dice? | la **CA de enrollment** (`ASP_CA_CERT`/`ASP_CA_KEY`, la misma que `ASP_CLIENT_CA`) | los certificados de los nodos, y el de cliente del propio plano de control (`CN=asp-control-plane`) |
| ¿Este es el plano de control? | **`--control-plane-ca`** en cada nodo | solo el certificado TLS del plano de control (`ASP_TLS_CERT`) |

Recomendado: firma el certificado del plano de control con una CA que no firme nada más ([las dos raíces](../ops-multi-node.md#las-dos-raíces-de-confianza)). Las demás claves: la clave RSA con que el plano de control firma los tokens de workload (`ASP_OIDC_KEY`; el JWKS público la publica), las claves ECDSA de las atestaciones (la del certificado del nodo, o `ASP_ATTEST_KEY`), y los tokens de enroll y las API keys, que se guardan con hash.

**No garantiza:** la CA de enrollment es la raíz de confianza de los nodos: quien tenga `ASP_CA_KEY` emite un nodo. Un nodo comprometido tiene root en su host: ve los discos y las claves de sus sandboxes y puede mentir en lo que informa.

### La API local del nodo

El agente expone en `127.0.0.1:9100` el `exec` de cualquier sandbox, la comprobación de la política de egress y las aprobaciones de firmas. Loopback **no es una credencial** (cualquier usuario local, o un proceso que logre que el proxy abra una conexión a `127.0.0.1`, llega a él): toda ruta menos `/healthz` pide el token de `--agent-token-file`, que solo leen root y el usuario del plano de control del mismo host. Un plano de control en otro host no usa este camino: llega por mTLS al puerto `--agent-tls-listen`, donde el agente solo acepta el certificado del plano de control. Fuera de loopback, el agente se niega a escuchar en HTTP salvo `--insecure-agent-listen`. Las métricas y `pprof` (sin autenticación) escuchan solo en loopback salvo `--insecure-obs-listen`.

### Atestación, fencing y nodos perdidos

**Atestación.** El nodo firma, al arrancar cada sandbox, una declaración con el digest del kernel y de la imagen base, la versión del hipervisor y si es un arranque nuevo o una reanudación; el plano de control solo acepta claves que conoce, y con `ASP_ATTEST_ALLOWED_IMAGES` solo imágenes de una lista. Es una firma **de software**, no TPM ni SEV: un nodo comprometido puede declarar lo que quiera con su propia clave. Prueba que el nodo dice haber arrancado esa imagen, no que lo hiciera.

**Fencing.** Un nodo que deja de dar señales sale del reparto a los 90 s y sus sandboxes pasan a `failed` a los 5 min, tras llamar a un *provider* que intenta apagarlo (un webhook funciona; Redfish e IPMI son stubs). La autodefensa del nodo (para toda VM que ya no esté en su conjunto `assigned`) **no es STONITH**: un nodo particionado sigue corriendo sus VMs hasta que vuelve o hasta el fencing.

## Lo que ASP no cubre

- Atestación de hardware (TPM, SEV), y guests que no sean Linux.
- Canales laterales entre microVMs de un mismo host, y agotamiento de recursos más allá de los límites de cgroup de cada VM.
- Vulnerabilidades de Cloud Hypervisor, KVM o el kernel (se reportan a esos proyectos; ASP cubre configurarlos mal).
- IPv6, QUIC y UDP arbitrario en el egress; el bypass-proof de nftables demostrado en CI.
- Fencing de producción por BMC validado de punta a punta.
- Un operador malicioso con root en el nodo o en el plano de control: son la base de confianza.
- NetworkPolicy de Kubernetes como frontera de egress: no sustituye al proxy y a nftables del nodo.
- La cadena de suministro de la imagen del guest más allá de comprobarla contra su `SHA256SUMS` (`--guest-verify`).

La lista de límites conocidos y de qué cuenta como vulnerabilidad: [SECURITY.md](../../SECURITY.md).
