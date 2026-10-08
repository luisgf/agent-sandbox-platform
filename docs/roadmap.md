# Hoja de ruta

Lo que viene. Lo que ya está hecho no se cuenta aquí: los cambios por versión están en el [CHANGELOG](../CHANGELOG.md), la historia de cómo se construyó el MVP por fases en [history.md](history.md), y lo que el código **no** hace hoy en [límites conocidos](reference/limitations.md). Cada punto dice dónde se sigue: una issue o un ADR. Una cosa que no está aquí no es un «no»: es que nadie ha decidido todavía.

## Para publicar la primera versión (0.1.0)

Las piezas están escritas y probadas: una release se instala en una Ubuntu y una Debian limpias, la segunda se une a la primera como nodo, y el kernel y la imagen del guest salen iguales en cada build (`scripts/e2e-install-kvm.sh`, [comprobar un nodo](how-to/e2e-kvm.md#el-instalador-en-máquinas-limpias)). Los workflows de release, de documentación y del guest (el kernel y la imagen, cada uno construido dos veces desde cero) están activos; lo que falta es ensayar la release, publicarla y un runner con KVM para el carril nocturno.

| Qué falta | Dónde |
|---|---|
| Ensayar el workflow de release (Actions → *Release* → *Run workflow*, no publica nada) y publicar `v0.1.0`: binarios, paquetes `.deb`/`.rpm`, la imagen del plano de control, el kernel y la imagen del guest con su `SHA256SUMS`. Después, `scripts/e2e-install-kvm.sh` con los ficheros de esa release: es la comprobación de que un host limpio la instala | #127, [publicar una versión](how-to/release.md) |
| Un carril de CI nocturno en un host con KVM que recorra el ciclo real. Los scripts están hechos y probados (`e2e-kvm.sh`, `smoke-vmm-user-kvm.sh`, `smoke-egress-kvm.sh`, `e2e-install-kvm.sh`); falta un runner con KVM y activar el workflow. Hoy KVM, virtiofs y `--local-net` no corren en CI (las reglas de nftables sí, en namespaces y sin guest: `smoke-egress-nft.sh`) | #130, [comprobar un nodo](how-to/e2e-kvm.md) |
| Un solo idioma para la documentación | #142 |

## Decisiones pendientes

Cada una acaba en un ADR o en una issue cerrada con su motivo; ninguna se da por tomada.

- **¿Y Kubernetes?** [ADR-0004](adr/0004-k8s-scope.md) decidió que las sandboxes no son Pods, sin examinar `agent-sandbox`, Kata con Cloud Hypervisor, Virtink ni E2B. Se reevalúa, con una medición, en el ADR-0013 (#143).
- **Arranque rápido de una sesión:** el [ADR-0017](adr/0017-fast-start.md) (propuesta) mide qué hay en los 6 s de un arranque y propone arreglar primero lo que sobra, sin construir ahora ni el pool de VMs precalentadas (#81) ni las instantáneas de Cloud Hypervisor (#144): el helper del guest que esperaba a un tag que no existe (hecho, −1,8 s), los discos como overlay qcow2 en vez de copias (−1,5 s, #204) y un sondeo de trabajo más corto (#205).

## Más adelante

Sin fecha y sin compromiso. Son lo que los ADRs y la revisión de diseño dejaron anotado como siguiente paso, agrupado por qué cambiaría.

**Seguridad**

- Atestación de hardware (TPM, SEV) detrás de la interfaz `Attestor`; hoy la firma es de software ([modelo de seguridad](concepts/security-model.md#atestación-fencing-y-nodos-perdidos)).
- Fencing de producción por BMC (Redfish, IPMI): hoy son stubs y solo funciona el webhook.
- `virtiofsd` sin root: exige traducir ids o un namespace de usuario por VM, y [ADR-0015](adr/0015-unprivileged-vmm.md) lo deja para aparte.
- **Atribución de los flujos de red a `owner_sub`** ([ADR-0008](adr/0008-network-flow-attribution.md)). Ya se reconoce la sandbox por su IP de origen y el audit del proxy lleva su `sandbox_id`; faltan `owner_sub` en esa línea, la identidad hacia un proxy corporativo y las marcas de nftables (`ct mark`). Criterio de aceptación: dos sandboxes de distinto `owner_sub` hacia el mismo destino dan líneas de audit distintas sin cabeceras del guest.
- IPv6 y QUIC en el egress, y reglas por sandbox al estilo de NetworkPolicy en el nodo.

**Operación**

- Un plano de control con más de una réplica de verdad: elección de líder para el monitor de nodos (hoy cada réplica corre el suyo y el fencing podría repetirse, [ADR-0011](adr/0011-multi-node.md)).
- SLOs y alertas sobre las métricas que ya hay ([monitorizar](how-to/monitoring.md)); pruebas de caos.
- Métricas de lo que hoy no tiene ninguna: revocaciones, firmas SSH denegadas, fallos del redirect de nftables.
- Copias de seguridad automáticas; hoy son un procedimiento a mano ([copias y restauración](how-to/backup-and-restore.md)).

**Producto**

- Un perfil de Firecracker ([ADR-0001](adr/0001-vmm-choice.md)): adaptador y mediciones, sin prometer paridad.
- Un workspace persistente cifrado.
- Excepciones estrechas en `--local-net` (hoy es todo o nada, [ADR-0010](adr/0010-on-demand-local-net.md)): solo con un caso de uso y un ADR nuevo.
- Un IdP corporativo probado (Entra ID, Okta) y la pertenencia a tenants en el esquema (`tenant_memberships`); hoy el tenant y el rol salen del token y se ha probado con Keycloak ([conectar un IdP](how-to/idp.md)).
- Generar clientes (el de Go del CLI, y los de otros lenguajes) a partir del [documento OpenAPI](reference/api.md), con `oapi-codegen` por ejemplo, en vez de escribirlos a mano (#139).

## Fuera de alcance

Decisiones tomadas, no pendientes:

- Ejecutar sandboxes como Pods de Kubernetes ([ADR-0004](adr/0004-k8s-scope.md)).
- Exponer microVMs directamente a Internet.
- Guardar claves privadas o refresh tokens en las imágenes del guest.
- Cargas que necesiten `NET_ADMIN` en el guest, y guests que no sean Linux.
- Decir que `soft` (nftables, TAP) equivale a `enforce` en producción.
- Tratar el UID Linux del guest como identidad humana ([ADR-0007](adr/0007-multi-user-identity.md)), ni las marcas o cabeceras que pone el guest como atribución de red ([ADR-0008](adr/0008-network-flow-attribution.md)).
- Tratar create→exec→destroy por comando de shell como la superficie de integración de un agente ([ADR-0009](adr/0009-agent-sessions.md)); `asp sandbox run` queda como primitiva de CI y operaciones.
- Abrir la LAN del usuario por defecto o con un port forward, o volver en silencio al proxy del nodo cuando cae el túnel de `--local-net` ([ADR-0010](adr/0010-on-demand-local-net.md)).
- Declarar el agente SSH seguro para varios usuarios sin `ASP_SSH_AGENT_SOCK_TEMPLATE` (el puente global es el comportamiento antiguo); con la plantilla y la confirmación, el aislamiento es por ruta y las claves siguen siendo cosa de operaciones ([ADR-0007](adr/0007-multi-user-identity.md)).
