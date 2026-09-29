# Arquitectura

## Resumen

La plataforma separa orquestación, ejecución privilegiada y carga invitada. El camino de control es:

```text
cliente → control-plane → node-agent → microVM → pod-daemon
```

La frontera de seguridad primaria es la microVM; los contenedores dentro del guest, si se incorporan, son una comodidad de empaquetado y no sustituyen esa frontera.


## Diagrama

![Arquitectura](diagram.svg)

Fuente Mermaid: [`diagram.mmd`](diagram.mmd).

## Capas

### 1. Cliente

CLI, IDE o servicio automatizado. Se autentica ante el control plane, solicita un sandbox, consulta su estado y abre operaciones de ejecución o archivos. Nunca habla directamente con el hipervisor ni recibe credenciales de infraestructura permanentes.

### 2. Control plane (Go)

Expone la API multi-tenant, valida autorización y cuotas, mantiene el estado deseado y el historial inmutable de eventos, elige un nodo compatible y entrega una orden firmada al node-agent. Publica JWKS para validar tokens OIDC. Puede desplegarse en Kubernetes, pero no ejecuta workloads de usuario.

### 3. Node agent (Go)

Proceso privilegiado por nodo. Prepara discos, TAP/NAT, cgroups y dispositivos; inicia Cloud Hypervisor; conecta vsock; registra salud y capacidad; y reconcilia estado real contra estado deseado. Es la única capa autorizada para invocar al VMM y para pedir tokens vinculados a una atestación válida.

### 4. microVM

Debian mínimo sin acceso al socket del runtime del host, sin `NET_ADMIN` y sin secretos persistentes. Recibe una NIC virtio restringida y vsock. La imagen base es inmutable; los cambios del sandbox viven en un overlay efímero o volumen explícito.

### 5. pod-daemon (Rust)

PID de servicio dentro del guest. Escucha por vsock en producción (socket Unix en desarrollo), implementará `Exec`, `ReadFile`, `WriteFile` y `Metrics`, y materializa sockets locales para el puente de SSH agent e identidad. No decide tenancy ni identidad.

## Ciclo de vida

Estados nominales:

```text
requested → scheduled → starting → running → paused → running → stopping → stopped
```

`failed` es terminal para un intento. Cada transición requiere control optimista de versión y genera un `sandbox_event`; los reintentos deben ser idempotentes. El node-agent reconcilia en vez de depender de una secuencia de RPC perfecta.

## Identidad

Hay dos canales y ningún secreto de larga duración dentro de la imagen:

1. **SSH:** el agente permanece en el host. El node-agent expone un proxy limitado por vsock que `pod-daemon` presenta como socket Unix invitado. Las claves privadas no cruzan la frontera.
2. **OIDC:** el proceso invitado hace `POST /v1/tokens/oidc` sobre un socket Unix administrado por `pod-daemon`, enviando únicamente `aud`. El node-agent valida la atestación y fija `tenant_id`, `sandbox_id`, nodo, expiración y políticas; el guest no puede elegirlos. El control plane firma tokens de corta vida y publica JWKS.

Todas las llamadas de servicio usan mTLS e identidades rotables. Las API keys externas se almacenan sólo como hash.

## Red y egress

Cada microVM usa TAP conectado a la red del nodo y NAT para conectividad saliente. No hay entrada directa desde Internet. El tráfico HTTP(S) y las consultas DNS pasan obligatoriamente por proxies del nodo. La política es **deny-by-default** y la allowlist se calcula por tenant/sandbox. El proxy registra destino, decisión y volumen sin registrar cuerpos ni secretos.

Los controles de red se aplican fuera del guest (nftables/eBPF, rutas y cgroups), porque el guest es carga no confiable. Véase [ADR-0002](adr/0002-networking.md).

## Datos y observabilidad

PostgreSQL conserva tenants, sandboxes, eventos, API keys y nodos. Artefactos y snapshots futuros viven en object storage cifrado. Métricas incluyen latencia de scheduling, tiempo de arranque, uso de CPU/memoria, denegaciones de egress y salud del vsock. Logs y trazas llevan `tenant_id`, `sandbox_id` y `request_id`, con redacción de secretos.

## Límites operativos

Kubernetes es opcional para el control plane. Los nodos de sandbox son bare metal o VMs administradas con node-agent; las microVMs no se modelan como Pods. Esto mantiene clara la propiedad de dispositivos, red y recuperación.

## Evolución

El orden de implementación, criterios de salida y endurecimiento están en [`roadmap.md`](roadmap.md). Las decisiones normativas están en [`adr/`](adr/).
