# Monitorizar ASP: métricas, logs y perfiles

El plano de control y cada node-agent exponen métricas en formato de texto de Prometheus, escriben un log estructurado con una línea por paso del ciclo de vida de cada sandbox y, si se pide, sirven los perfiles de Go. Nada de esto está activado hacia fuera por defecto.

## Activarlo

| Qué | Plano de control | Node-agent |
|---|---|---|
| Métricas, en el puerto del API | `GET /metrics` en `LISTEN_ADDR`. Pide una **clave API de plataforma** o un token del IdP con rol `admin` u `operator` (las cifras abarcan a todos los tenants); el lab sin claves la deja abierta, como `GET /v1/nodes` | no hay API de red: ver la fila siguiente |
| Métricas, en un puerto aparte | `ASP_METRICS_LISTEN=127.0.0.1:9101` | `--metrics-listen=127.0.0.1:9102` (`ASP_METRICS_LISTEN`) |
| Perfiles de Go (`/debug/pprof/`) | `ASP_PPROF_LISTEN=127.0.0.1:6060` | `--pprof-listen=127.0.0.1:6061` (`ASP_PPROF_LISTEN`) |
| Escuchar fuera de loopback | `ASP_INSECURE_OBS_LISTEN=1` | `--insecure-obs-listen` (`ASP_INSECURE_OBS_LISTEN=1`) |

Los puertos aparte **no tienen autenticación**: por eso solo aceptan una dirección de loopback y el proceso no arranca (código 2) con otra salvo que se pida lo contrario. Para que un Prometheus de otra máquina los lea, ponles delante un proxy que autentique (nginx con una cabecera, un túnel SSH, WireGuard) en vez de abrirlos. `pprof` puede volcar la memoria del proceso: no lo expongas.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: asp-control-plane
    scheme: https
    metrics_path: /metrics
    authorization:
      credentials_file: /etc/prometheus/asp-platform-key   # una clave API de plataforma
    static_configs:
      - targets: ['cp.example.corp:8443']
  - job_name: asp-node-agent
    static_configs:
      - targets: ['node1.example.corp:9102']               # detrás de tu proxy o túnel
```

## Plano de control (`asp_*`)

| Métrica | Tipo | Etiquetas | Qué dice |
|---|---|---|---|
| `asp_http_requests_total` | contador | `route`, `code` | Peticiones por **patrón de ruta** (`POST /v1/sandboxes/{id}/exec`), nunca por la ruta cruda, y código de estado. Una ruta que no existe cuenta como `unmatched`. |
| `asp_http_request_duration_seconds` | histograma | `route` | Duración. Un `exec` en streaming dura lo que su comando. |
| `asp_http_requests_in_flight` | gauge | | Peticiones en curso (los `exec` en streaming incluidos). |
| `asp_sandbox_creates_total` | contador | `result` | `ok`, `no_capacity` (503), `node_unavailable` (409), `invalid`, `error`. |
| `asp_sandboxes` | gauge | `tenant`, `state` | Sandboxes por tenant y estado, las borradas incluidas. Una consulta agrupada por lectura. |
| `asp_sandboxes_reaped_total` | contador | `reason` | Paradas o borrados del propio plano de control: `idle_timeout`, `retention_expired`, `tenant_cap`. |
| `asp_sandboxes_lost_total` | contador | `reason` | Sandboxes falladas por perder su nodo (`node_lost`). |
| `asp_node_events_total` | contador | `event` | `offline`, `fenced`, `fence_failed`. |
| `asp_node_up` | gauge | `node` | 1 si el nodo dio señales de vida dentro de la ventana y no está revocado. |
| `asp_node_schedulable` | gauge | `node` | 1 si el planificador pondría una sandbox ahí. |
| `asp_node_cordoned`, `asp_node_egress_enforced` | gauge | `node` | 1 si está acordonado / si fuerza el egress de sus guests. |
| `asp_node_sandboxes`, `asp_node_stopped_sandboxes` | gauge | `node` | Sandboxes que ocupan capacidad / paradas con su disco en el nodo. |
| `asp_node_allocated_cpu_millis`, `asp_node_allocated_memory_mib` | gauge | `node` | Lo asignado. |
| `asp_node_last_seen_age_seconds` | gauge | `node` | Segundos desde la última señal de vida. |
| `asp_node_disk_free_bytes` | gauge | `node` | Espacio libre del directorio de discos, como lo informó el nodo. |
| `asp_db_pool_connections` | gauge | `state` | Pool de Postgres: `acquired`, `idle`, `total`, `max`. |
| `asp_db_pool_acquires_total`, `…_empty_acquires_total`, `…_canceled_acquires_total`, `…_acquire_wait_seconds_total` | contador | | Cuántas veces se pidió una conexión, cuántas tuvieron que esperar, cuántas se abandonaron y cuánto se esperó en total. |
| `asp_collector_errors_total` | contador | `collector` | Lecturas de la métrica que fallaron (`sandboxes`, `nodes`). |
| `go_*`, `process_*` | | | Goroutines, memoria, GC, CPU, descriptores abiertos, hora de arranque. |
| `asp_metrics_dropped_series_total` | contador | | Observaciones descartadas porque una métrica llegó a su tope de 1000 series (una etiqueta sin cota). Debe ser 0. |

## Node-agent (`asp_agent_*`)

| Métrica | Tipo | Etiquetas | Qué dice |
|---|---|---|---|
| `asp_agent_vm_starts_total` | contador | `kind`, `result` | Arranques (`kind`: `new` o `resume`) y su resultado: `ok`, `setup` (antes de la VMM: claim, red, workspace, disco), `vmm_start`, `guest_not_ready`, `vmm_exited`, `report`. |
| `asp_agent_vm_start_seconds` | histograma | `kind` | Del claim al informe `running`; solo arranques que acabaron bien. |
| `asp_agent_guest_ready_seconds` | histograma | `result` | Lo que esperó un arranque a que el pod-daemon del guest respondiera (`ready` o `timeout`). |
| `asp_agent_vms` | gauge | | VMs que el nodo corre o está arrancando. |
| `asp_agent_vm_stops_total` | contador | `how` | VMs liberadas: `stop`, `delete`, `fence`, `exited`, `start_failed`, `panic`. |
| `asp_agent_vm_exits_total` | contador | `cause` | Procesos de la VMM que acabaron solos: `crash` (muerto o fallido, el OOM killer incluido) o `poweroff` (el guest se apagó). |
| `asp_agent_disk_free_bytes` | gauge | | Espacio libre de `--disk-dir`: lo que se agota primero con sandboxes paradas. |
| `asp_agent_disk_gc_removed_total` | contador | | Discos que el GC borró por no tener dueño. |
| `asp_agent_poll_failures_total` | contador | | Sondeos al plano de control que fallaron. |
| `asp_agent_vms_adopted_total` | contador | `result` | VMs que un proceso anterior del agente dejó corriendo: `ok` (adoptadas) o `stale` (ya no vivían o no se pudieron adoptar; se limpian). |
| `asp_agent_egress_enforced` | gauge | | 1 si el nodo fuerza el egress de sus guests (proxy + reglas nft en `enforce`). |
| `asp_agent_egress_requests_total` | contador | `decision`, `reason`, `tenant` | Decisiones del proxy: `allow` (`http` o `connect`) o `deny` (`allowlist`, `destination_blocked`, `rate_limited`…), por tenant de la sandbox; `none` para un origen que no es sandbox. |
| `asp_agent_egress_bytes_total` | contador | `direction` | Bytes que acarrea el proxy: `to_upstream` desde el guest, `from_upstream` de vuelta. |
| `asp_agent_egress_dns_queries_total` | contador | `decision`, `tenant` | Consultas del DNS sink: `allow`, `deny` (NXDOMAIN), `error`, `unsupported`. |
| `go_*`, `process_*`, `asp_metrics_dropped_series_total` | | | Como en el plano de control. |

Las etiquetas `tenant` y `node` están acotadas por el despliegue; ninguna etiqueta toma un id de sandbox ni nada que un cliente pueda inventar.

## Qué vigilar

```promql
# El plano de control no responde bien
sum(rate(asp_http_requests_total{code=~"5.."}[5m])) / sum(rate(asp_http_requests_total[5m])) > 0.05
histogram_quantile(0.99, sum by (le, route) (rate(asp_http_request_duration_seconds_bucket{route!~".*exec.*"}[5m]))) > 2

# Un nodo no da señales de vida, o no puede recibir sandboxes
asp_node_up == 0
asp_node_up == 1 and asp_node_schedulable == 0 and asp_node_cordoned == 0

# Sin hueco para crear: los 503 ya son visibles antes de que lo diga un usuario
increase(asp_sandbox_creates_total{result="no_capacity"}[15m]) > 0

# Arranques que fallan, y VMs que mueren solas
sum by (result) (increase(asp_agent_vm_starts_total{result!="ok"}[15m])) > 0
increase(asp_agent_vm_exits_total{cause="crash"}[1h]) > 0

# El disco de las sandboxes paradas se acaba
asp_agent_disk_free_bytes < 20 * 1024^3

# Un nodo con VMs reales que no fuerza el egress: la política del tenant no obliga a sus guests
asp_node_egress_enforced == 0

# Postgres: el pool está exhausto o se espera mucho por una conexión
asp_db_pool_connections{state="acquired"} / asp_db_pool_connections{state="max"} > 0.9
rate(asp_db_pool_acquire_wait_seconds_total[5m]) > 0.1
```

## Logs

Ambos procesos escriben con `log/slog` (texto por stderr; con systemd, en el journal).

- **Una línea por paso del ciclo de vida de una sandbox**, con los mismos campos siempre, de modo que su historia es un `grep` por su id. El plano de control escribe `sandbox lifecycle event=created|reported|stop_requested|resume_requested|delete_requested sandbox_id=… tenant=… state=… node_id=…` (los informes del nodo añaden `detail` y `stop_reason`); el nodo, `sandbox running`, `sandbox stopped`, `the VM ended on its own`… con `sandbox_id`.
- Que un cliente corte un stream (un `| head`, Ctrl-C) se registra a nivel **INFO** (`…: the client went away`); `ERROR` queda para fallos del agente o del guest.
- Las peticiones: `request method=… path=… status=… duration=…`; los sondeos y latidos de los nodos y `/healthz`, a DEBUG.

```bash
journalctl -u asp-control-plane | grep 'sandbox_id=4252d313'
journalctl -u asp-node-agent    | grep 'sandbox_id=4252d313'
```

## Perfiles

Con `ASP_PPROF_LISTEN` / `--pprof-listen` en loopback:

```bash
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
go tool pprof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'
curl -s http://127.0.0.1:6060/debug/pprof/goroutine?debug=2
```
