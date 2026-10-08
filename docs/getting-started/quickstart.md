# Empezar

Tres caminos, según lo que tengas. Los dos primeros acaban en una sesión que ejecuta comandos; el tercero, en varios servidores.

| Tienes | Haz | Guía |
|---|---|---|
| **Un host Linux con KVM** y quieres todo en él (el caso normal para probar ASP en serio) | Instalar `asp-server`: un plano de control y un nodo en un proceso, con la base de datos, las claves y el certificado hechos solos | [Un solo host](../how-to/single-host.md) · [instalar con el script](../how-to/install.md) |
| **Un portátil o CI, sin KVM** | El *dry-run*: recorre todo el camino de control con una VM de mentira. **No aísla nada** | [Más abajo](#sin-kvm-dry-run) |
| **Varios servidores** | Un plano de control con Postgres y un nodo por servidor | [Instalar el plano de control](../how-to/install-control-plane.md) · [instalar un nodo](../how-to/install-node.md) · [varios servidores](../ops-multi-node.md) |

Cuando tengas una sesión, el siguiente paso es enganchar el shell de un agente a ella: [usar ASP con OpenCode](opencode.md). Qué hace la sesión por dentro: [cómo funciona una sesión](../concepts/sessions-and-lifecycle.md).

## En un host con KVM

Con una [versión publicada](../how-to/release.md) no hace falta compilar nada:

```bash
curl -fsSL https://github.com/luisgf/agent-sandbox-platform/releases/latest/download/install.sh | sudo INSTALL_ASP_ROLE=standalone sh
sudo asp session start
sudo asp session exec --cmd 'uname -a'
```

El script comprueba cada descarga contra el `SHA256SUMS` de la release, instala los paquetes (con `nftables`), instala Cloud Hypervisor (la versión probada, con su suma fijada en el script) y `virtiofsd` si la distribución lo tiene, baja el kernel y la imagen del guest, y arranca `asp-server`, que hace la base de datos (SQLite), las claves y un certificado TLS autofirmado, ejecuta un plano de control (sin privilegios) y un nodo, y deja el comando `asp` del host configurado. Las variables y el recorrido con dos hosts: [instalar con el script](../how-to/install.md). **Todavía no hay ninguna versión publicada** (la primera será la 0.1.0, [hoja de ruta](../roadmap.md)); hasta entonces, compila (`make build`) y sigue [un solo host, a mano](../how-to/single-host.md#sin-systemd-o-a-mano) o las guías de [un nodo](../how-to/install-node.md) y del [plano de control](../how-to/install-control-plane.md).

## Sin KVM (dry-run)

El *dry-run* usa `FakeVMM`: ejercita todo el camino de control en un portátil o en CI. **No da aislamiento.** Para microVMs de verdad, [instalar un nodo](../how-to/install-node.md).

**Requisitos:** Go 1.26 o posterior (cada módulo declara el suyo en su `go.mod`) y Rust/Cargo. Docker solo para Postgres o para construir el rootfs del guest.

### 1. Compila y prueba

```bash
make test        # pruebas unitarias de Go y de Rust
make smoke       # los scripts de enroll / identidad / reconcile / dos nodos
make asp         # compila ./build/asp
make build       # todos los binarios: build/asp, build/asp-server, build/api (plano de control), build/node-agent (y pod-daemon con cargo)
make smoke-asp   # el CLI de extremo a extremo en dry-run
```

### 2. Arranca los tres procesos

```bash
export ASP_NODE_BOOTSTRAP_TOKEN=dev-node-bootstrap

# terminal 1 — plano de control (almacén en memoria, 127.0.0.1:8080). La autenticación está
# siempre activa; este dry-run no tiene claves, así que la desactiva expresamente (solo
# laboratorios y portátiles).
(cd control-plane && ASP_INSECURE_OPEN_API=1 go run ./cmd/api)

# terminal 2 — pod-daemon en un socket unix (hace de guest)
(cd pod-daemon && cargo run -- --listen unix --unix-socket /tmp/pod-daemon.sock)

# terminal 3 — node-agent en dry-run
(cd node-agent && go run ./cmd/node-agent \
  --control-plane-url=http://127.0.0.1:8080 --node-id=dev-node \
  --dry-run --enroll --bootstrap-token=dev-node-bootstrap \
  --cert-dir=/tmp/asp-node-certs --agent-listen=127.0.0.1:9100 \
  --pod-daemon-sock=/tmp/pod-daemon.sock --reconcile \
  --host-vsock --host-vsock-dir=/tmp/asp-hv)
```

### 3. Abre una sesión y ejecuta comandos

```bash
./build/asp session start --name demo
./build/asp session exec  --name demo --cmd 'uname -a'
./build/asp session status --name demo
./build/asp session stop  --name demo     # conserva el disco; `resume` la arranca otra vez, `rm` la borra
```

El plano de control coloca la sesión en un nodo con hueco; con el único nodo del dry-run, es `dev-node`. `--node-id` fija uno.

**Persistencia (opcional):** `docker compose up -d postgres` y arranca el plano de control con `ASP_DATABASE_URL=postgres://asp:asp@127.0.0.1:5432/asp?sslmode=disable`. Sin ella, el estado se pierde al salir el plano de control.

El recorrido paso a paso y los fallos habituales: [el smoke de dry-run](../mvp-smoke.md).
