# ADR-0016: Un solo host: SQLite y `asp-server`, un servidor que se configura solo

- **Estado:** Aceptada (2026-10, #124)
- **Contexto:** [ADR-0011](0011-multi-node.md) (nodos y colocación), [ADR-0004](0004-k8s-scope.md) (el plano de control puede ir fuera del nodo), [#125](../how-to/config-file.md) (fichero de configuración)

## Contexto

Poner ASP en marcha en un servidor eran unos 24 pasos a mano (`bare-metal-ch.md`): compilar o bajar tres programas, un Postgres en Docker como única forma de que el estado sobreviviera a un reinicio, la CA, las claves OIDC y de atestación (por defecto en `/tmp`, y por defecto un API abierta hasta #95), el token de alta del nodo, el certificado TLS, una clave de administración y el enlace de todo ello con la unit. El modelo a copiar es k3s: un instalador de una línea, un servidor que funciona sin configuración, SQLite por defecto y agentes que se unen con una URL y un token. El objetivo: **tres comandos hasta la primera sesión en un host, dos para añadir otro**.

## Decisión

1. **Un tercer store, SQLite** (`modernc.org/sqlite`, sin CGO), con `ASP_DATABASE_URL=sqlite:///var/lib/asp/server/asp.db`. Sigue el store de Postgres sentencia a sentencia y cumple **el mismo contrato**: el guion de paridad (todos los métodos de `Store`, mismo resultado y misma clase de error) corre contra él, los tests de `internal/store` que solo usan la interfaz son funciones compartidas con una variante por store, y la suite de `internal/api` tiene una tercera pasada. A diferencia de Postgres no necesita ningún servidor, así que **cada ejecución de tests, CI incluido, lo cubre**. Postgres sigue siendo el store de más de un plano de control; SQLite es un fichero de un host.
   - Una sola conexión y transacciones `BEGIN IMMEDIATE`: todo está serializado, que es lo que en Postgres hacen el advisory lock de la colocación y `FOR UPDATE`. Dos procesos sobre un fichero se ponen en cola.
   - Los tiempos son TEXT de un ancho fijo (UTC, microsegundos): comparar el texto es comparar el instante, y un test comprueba cada columna.
   - El esquema es una sola migración (`migrations/sqlite/024_baseline.sql`, el de Postgres hasta la 024); desde la 025 cada migración tiene su gemela, y `TestSQLiteMigrationsMatchPostgres` falla si no.
2. **`asp-server` es un supervisor de procesos, no un proceso único.** Hace lo que un host necesita (directorios, certificado TLS autofirmado, clave de administración, token del nodo, el secreto de la API local del nodo) y arranca `asp-control-plane` y `asp-node-agent` tal como los instalan los paquetes. No es un solo ejecutable porque son módulos Go distintos con paquetes `internal/` que no se importan entre sí, porque el nodo tiene que poder reiniciarse sin llevarse por delante las VMs ni el plano de control, y porque el plano de control corre **sin privilegios** (`asp-control-plane`) y el nodo como root: en un solo proceso no se separarían. Cada programa conserva sus ajustes, su paquete y sus tests; `asp-server` solo decide lo que ya no hay que decidir a mano y lo pone en su entorno (el entorno gana al fichero, [#125](../how-to/config-file.md)).
   - Orden: el plano de control primero; el nodo cuando responde; al parar, el nodo primero. Un programa que sale con 2 (rechazó un ajuste) no se reinicia: se para todo y se dice cuál.
   - Se instala como un cuarto paquete, `asp-server`, que depende de los otros tres y con una unit que declara `Conflicts=` con los servicios sueltos y `KillMode=mixed` (la señal va solo a `asp-server`, que para a los demás en orden).
3. **Seguro por defecto.** El plano de control escucha en el loopback (`--listen 0.0.0.0:8443` para abrirlo a otros hosts); la autenticación está siempre activa; el nodo se da de alta por mTLS como cualquier otro; el certificado cubre el nombre del host, el loopback y lo que diga `--tls-san` (y todas las direcciones del host si escucha en todas). Los ficheros con secretos son 0600 y la base de datos se crea privada antes de que SQLite la toque. La clave de administración es un fichero `root:asp 0640`.
4. **El comando `asp` del host queda configurado sin guardar la clave en un fichero que lea todo el mundo.** `asp-server` escribe `/etc/asp/asp.yaml` (sin pisar uno existente) con la URL, `ca_file` (el certificado autofirmado, que el CLI acepta además de los del sistema) y `api_key_file` (la ruta de la clave). Lo lee quien pueda leer la clave: root y el grupo `asp`.
5. **Unirse a un servidor con certificado propio sin copiar ficheros.** `asp node enroll-token` imprime, con el token, el comando del instalador con `INSTALL_ASP_CA_SHA256`: el instalador lee el certificado del servidor, **se niega a seguir si su huella no coincide** (antes de instalar nada) y solo entonces lo toma como confianza. Es la comprobación de huella de k3s (`--token` con el hash de la CA), sin inventar un formato de token: el token sigue siendo el de un solo uso de [ADR-0011](0011-multi-node.md).
6. **`--profile lab`**: el nodo sin VMs (`--dry-run`), para probar sin KVM y para el smoke de CI. Conserva todo lo demás (TLS, autenticación, alta por mTLS, SQLite): lo que se prueba es el flujo real.

## Alternativas

- **Postgres embebido o un contenedor de Postgres gestionado por `asp-server`.** Más piezas que fallan y un requisito (Docker) que el objetivo quería quitar.
- **Solo SQLite, quitando el store en memoria y reduciendo Postgres.** El de memoria es el de los tests unitarios y el de los smokes sin estado; Postgres es el camino a más de un plano de control. Tres implementaciones de un contrato cuestan un test de paridad y una gemela por migración; se aceptó.
- **Un solo proceso que enlace el plano de control y el nodo.** Habría que publicar paquetes hoy `internal/`, y se perdería la separación de privilegios y la independencia de reinicio.
- **Un formato de token de unión con la huella dentro (`K10<hash>::<secreto>`, como k3s).** Un formato nuevo que parsear en el nodo y en el instalador, frente a una variable más en la línea del instalador que ya imprime el CLI.
- **La clave de administración en `/etc/asp/asp.yaml`.** Lo leería todo el mundo.

## Consecuencias

- Un método nuevo de `Store` necesita un paso del guion de paridad (ya lo exigía) y su versión SQLite; una migración nueva, su gemela. El test de cobertura del guion y el de migraciones lo hacen fallar si se olvida.
- `asp-server` y el nodo son root. El plano de control no, pero comparte host con él: quien lo comprometa no tiene root, y sí la CA, las claves y la base de datos. Para una instalación con más exigencias, los servicios por separado y, sobre todo, nodos que no comparten host con el plano de control.
- El plano de control de un host es único. La alta disponibilidad es Postgres y varios planos de control, como antes.
- El comando de copia de seguridad de la base de datos es `sqlite3 … ".backup"`, que no instalamos; `docs/how-to/single-host.md` lo explica.
