# Documentación de ASP

Un índice con una línea por página. Las páginas están agrupadas por lo que buscas, no por dónde viven en el árbol: **empezar** (un camino hasta una sesión que funciona), **conceptos** (cómo funciona y por qué), **guías** (cómo se hace una tarea) y **referencia** (lo que se consulta). Los ADRs y el laboratorio van aparte. La portada del proyecto es el [README](../README.md); cómo colaborar, [CONTRIBUTING.md](../CONTRIBUTING.md). Los documentos de `docs/` están en español; el README, el CHANGELOG y SECURITY.md, en inglés.

**Según quién seas**

| Si… | Empieza por |
|---|---|
| quieres **probar ASP** | [Empezar](getting-started/quickstart.md), y [cómo funciona una sesión](concepts/sessions-and-lifecycle.md) |
| vas a **enganchar un agente** (OpenCode u otro harness) | [Usar ASP con OpenCode](getting-started/opencode.md), [sesiones](ops-asp-session.md), [la API](reference/api.md) |
| **operas** un despliegue | [Instalar](how-to/install.md), [actualizar](how-to/upgrade.md), [copias y restauración](how-to/backup-and-restore.md), [monitorizar](how-to/monitoring.md), [diagnosticar](how-to/troubleshooting.md) |
| **evalúas la seguridad** | [El modelo de seguridad](concepts/security-model.md), [límites conocidos](reference/limitations.md), los [ADRs](adr/README.md) |
| vas a **tocar el código** | [CONTRIBUTING.md](../CONTRIBUTING.md), [la arquitectura](architecture.md), el [glosario](reference/glossary.md) |

## Empezar

- [Empezar](getting-started/quickstart.md): los tres caminos (un host con KVM, un portátil sin KVM, varios servidores) y el *dry-run* paso a paso.
- [Usar ASP con OpenCode](getting-started/opencode.md): el wrapper, el plugin y las configuraciones, en dos montajes; qué corre en la sandbox y qué no.

## Conceptos

- [Arquitectura](architecture.md): los componentes, las capas y los flujos, con el diagrama ([`diagram.svg`](diagram.svg), de [`diagram.mmd`](diagram.mmd)).
- [Cómo funciona una sesión, y la vida de una sandbox](concepts/sessions-and-lifecycle.md): de abrirla a cerrarla, y por qué estados pasa.
- [Cómo ejecuta un nodo las sandboxes](concepts/node-runtime.md): una microVM por sandbox, vsock, el workspace, reinicios del agente y confinamiento del VMM.
- [Red de las sandboxes y egress](concepts/networking-and-egress.md): el TAP, el proxy, el redirect de nftables, `--local-net`.
- [El modelo de seguridad](concepts/security-model.md): qué protege ASP, de quién, y qué garantiza y qué no cada frontera.
- [ASP y Kubernetes](concepts/asp-vs-kubernetes.md): qué son `agent-sandbox`, Kata, KubeVirt y E2B, y cuándo conviene cada uno.

## Guías

*Instalar*

- [Instalar con el script](how-to/install.md): `install.sh`, sus variables y el recorrido con dos hosts.
- [ASP en un solo host](how-to/single-host.md): `asp-server`, un plano de control y un nodo que se configuran solos.
- [Instalar el plano de control a mano](how-to/install-control-plane.md): Postgres, TLS, la CA y las credenciales.
- [Instalar un nodo a mano](how-to/install-node.md): KVM, Cloud Hypervisor, la imagen del guest y el servicio.
- [Varios servidores](ops-multi-node.md): capacidad, colocación, cordon, nodos caídos, añadir un servidor.
- [Operación bare-metal (índice)](bare-metal-ch.md): la guía antigua, partida; dice dónde está cada tema.

*Operar*

- [Actualizar ASP](how-to/upgrade.md): el orden, qué hace cada reinicio, cómo comprobar y cómo volver atrás.
- [Copias de seguridad y restauración](how-to/backup-and-restore.md): qué guardar, cómo, y qué hacen los nodos con una base restaurada.
- [Monitorizar](how-to/monitoring.md): métricas de Prometheus, logs y perfiles.
- [Diagnosticar un nodo](how-to/troubleshooting.md): `asp doctor` y la tabla de síntomas.
- [Operaciones de seguridad](how-to/security-operations.md): la lista de producción, rotaciones, mTLS estricto, atestación, fencing.
- [El fichero de configuración](how-to/config-file.md): YAML y *drop-ins*, con ejemplos.
- [Conectar un IdP (OIDC)](how-to/idp.md): roles, claims, y un ejemplo con Keycloak.
- [Publicar una versión](how-to/release.md): la etiqueta, goreleaser y lo que lleva una release.
- [Comprobar un nodo o una actualización de extremo a extremo](how-to/e2e-kvm.md): el ciclo real con KVM.

*Usar*

- [Sesiones de agente (`asp session`)](ops-asp-session.md): el contrato para un harness, las opciones y los fallos.
- [One-shot (`asp sandbox run`) y autenticación](ops-asp-agent-runner.md): la primitiva de CI y cómo el CLI obtiene el Bearer.
- [Red local bajo demanda (`--local-net`)](ops-local-net.md): el túnel hasta el portátil.
- [El smoke del *dry-run*](mvp-smoke.md): el camino de control sin KVM, con enroll y mTLS opcionales.

## Referencia

- [La API del plano de control](reference/api.md): las rutas, los cuerpos, los estados y quién puede llamar, generada de un documento OpenAPI.
- [Las órdenes de `asp`](reference/cli.md): cada orden con sus opciones, generada del código.
- [Configuración](reference/configuration.md): las reglas comunes, y las tablas de ajustes de cada programa: [plano de control](reference/configuration/control-plane.md), [node-agent](reference/configuration/node-agent.md), [`asp`](reference/configuration/cli.md) y [`asp-server`](reference/configuration/asp-server.md).
- [Glosario](reference/glossary.md): las palabras del proyecto.
- [Límites conocidos](reference/limitations.md): lo que ASP no hace hoy.

## Decisiones y rumbo

- [ADRs](adr/README.md): las decisiones de diseño, con su estado y sus consecuencias.
- [Hoja de ruta](roadmap.md): lo que viene.
- [Historia de las fases](history.md): cómo se construyó el MVP.

## El laboratorio

- [El laboratorio](lab/README.md): el host de pruebas de los mantenedores (no una guía de despliegue): [el host](lab/ncc1701d.md), [su Keycloak](lab/idp-keycloak.md).
