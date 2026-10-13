# ADR-0018: Un navegador y un escritorio sencillo dentro de la sandbox, medidos

- **Estado:** Propuesta
- **Fecha:** 2026-10-13
- **Implementación:** un estudio. En la rama `spike/browser-desktop`: la variante `desktop` de la imagen del guest (`ASP_GUEST_VARIANT=desktop scripts/build-guest-image.sh`). Nada de lo que la decisión pide antes de ofrecerla está hecho.
- **Relacionados:** [ADR-0001](0001-vmm-choice.md) (Cloud Hypervisor y «un camino creíble hacia desktop»), [ADR-0002](0002-networking.md) (el egress), [ADR-0010](0010-on-demand-local-net.md) (red local), [ADR-0013](0013-kubernetes-reevaluation.md) (el público de ASP), [ADR-0017](0017-fast-start.md) (arrancar rápido, [#204](https://github.com/luisgf/agent-sandbox-platform/issues/204)), [#231](https://github.com/luisgf/agent-sandbox-platform/issues/231)

## Contexto

Un agente necesita cada vez más un navegador de verdad: para leer páginas que solo existen con JavaScript, para probar la aplicación web que está construyendo, o para que una persona vea lo que hace y le eche una mano. Hoy el guest es una Debian 12 sin pantalla (1 vCPU y 512 MiB por defecto) cuya única salida es el proxy con lista blanca del host y su sumidero de DNS. Un navegador es el programa más grande y más hablador que se le puede meter.

[ADR-0001](0001-vmm-choice.md) eligió Cloud Hypervisor en parte por «un camino creíble hacia desktop», pero nadie lo había medido. Lo que había que averiguar ([#231](https://github.com/luisgf/agent-sandbox-platform/issues/231)):

- **La pantalla.** Cloud Hypervisor no tiene dispositivo gráfico: su modelo de dispositivos es bloque, red, consola, vsock, virtio-fs, rng, pmem y VFIO, y la única vía a virtio-gpu es un parche de Spectrum, fuera del árbol, que el propio Spectrum no cree que vaya a entrar. Firecracker tampoco tiene. Así que la pantalla se dibuja en memoria dentro del guest y se saca por la red, que es lo que hacen E2B Desktop, Daytona y la imagen de referencia de *computer use* de Anthropic (Xvfb, un gestor de ventanas, x11vnc y noVNC).
- **El coste.** Cuánto ocupa la imagen, cuánta memoria hace falta, cuánto tarda en arrancar hasta que el navegador contesta y cuántas caben en un host.
- **El egress.** Que lo permitido cargue, lo denegado no, y que nada se cuele por fuera del proxy: QUIC, DNS sobre HTTPS, WebRTC o el tráfico de fondo hacia Google.
- **El acceso.** Cómo llega una persona a la pantalla de su sandbox, y un agente a las DevTools, sin que llegue nadie más.
- **El empaquetado.** Una imagen aparte o una capa sobre la de hoy, y qué construiría el CI.

## Cómo se midió

**Dónde.** Como en [ADR-0013](0013-kubernetes-reevaluation.md): en el host de pruebas (ncc1701d, Xeon E-2236, 6 núcleos y 12 hilos, 32 GB) y sin tocarlo. Una VM Ubuntu 24.04 desechable (la cloud image del proveedor, Cloud Hypervisor con **KVM anidado**, 10 vCPU y 20 GiB) en un network namespace propio, con ASP instalado por su instalador de un solo host (`INSTALL_ASP_ROLE=standalone`, 40 s). Para que el proxy de ASP tuviera salida, los sitios de las pruebas resolvían a una dirección que el namespace del laboratorio redirigía a un reenviador, y este al proxy con lista blanca del laboratorio. **El proxy de ASP decide primero.**

**Qué.** ASP en el commit `bb9ad4a` y las dos imágenes construidas desde la rama del estudio con el kernel de producción (6.18.55): la normal y la variante `desktop`. Cada medida va desde la petición hasta que corre un comando, o hasta que el navegador contesta por DevTools. Los tiempos son medianas de 5 arranques, 3 cargas de página o 20 `exec`. Los números absolutos llevan la virtualización anidada encima ([ADR-0013](0013-kubernetes-reevaluation.md): 3,1 s en metal frente a 5,1 s anidado); los relativos entre las dos imágenes valen.

## La variante

| Pieza | Qué | Por qué |
|---|---|---|
| Pantalla | `Xvnc` de TigerVNC 1.12: servidor X y servidor VNC en un solo proceso, en el loopback del guest | lo más ligero y probado; nada dibuja en una GPU |
| Ventanas | Openbox | menús y diálogos se pueden mover; 1,3 MB |
| Navegador | Chromium 154 de Debian 12 (`bookworm-security`), con su sandbox | un `.deb` de verdad (no el snap de Ubuntu), al día en la foto del archivo |
| Para una persona | noVNC 1.3 y websockify, en el loopback del guest (6080) | se ve en cualquier navegador, sin cliente VNC |
| Para un agente | DevTools Protocol en el loopback del guest (9222) | Playwright, Puppeteer o un cliente propio |
| Salida | dos puentes `socat` de vsock (6080 y 9222) al loopback | el nodo llega a ellos como llega al pod-daemon (26500), por el socket de Cloud Hypervisor; el guest no abre ningún puerto de red |
| Política | `/etc/chromium/policies/managed/asp.json`: sin QUIC, sin DNS sobre HTTPS, sin métricas, sin Safe Browsing, sin actualizaciones de componentes, sin sincronización, WebRTC solo por el proxy | menos ruido en el proxy; **la frontera sigue siendo el nodo** |

Todo arranca con el guest (`asp-desktop.target`) como el usuario `sandbox` (uid 1000), con la pantalla a 1280x800. El navegador usa el proxy del nodo (`HTTPS_PROXY`, que el nodo ya pone en la línea de comandos del kernel). Ninguna línea de ASP cambió: es una imagen distinta.

## Resultados

### La imagen

| | Normal | Escritorio |
|---|---|---|
| Sistema de ficheros | 512 MiB | 2048 MiB (quedan 797 MiB libres dentro) |
| Ocupado en el disco del nodo, y en cada copia por sandbox | 119 MiB | **1114 MiB** |
| Comprimida (lo que baja `asp image pull`) | 39 MiB | 380 MiB (19 s en instalarse en el laboratorio) |
| Paquetes | 122 | 406 |

Los 972 MiB que añade la variante, por quién los trae:

| Qué | MiB | Quién lo pide |
|---|---|---|
| Chromium | 343 | — |
| Mesa y LLVM (OpenGL por software) | 166 | Chromium (por `libgbm1`) y el servidor X (por `libgl1`) |
| GTK 3 y su entorno | 83 | Chromium |
| Python 3 y sus módulos | 66 | websockify, noVNC y Openbox |
| Node.js | 53 | el paquete `novnc` de Debian |
| X, TigerVNC y Openbox | 39 | — |
| Fuentes | 23 | — |
| El resto (ICU, Perl, Ghostscript, datos de IEEE…) | 198 | dependencias de dependencias |

**Lo que sobra**, según lo que `apt` quitaría de verdad (con `--auto-remove`):

- **Servir noVNC desde el guest cuesta 73 paquetes y 128 MiB**: Node.js, NumPy, Pillow y las bibliotecas de OpenStack que arrastra websockify. noVNC son ficheros estáticos de 1 MB, y lo que hace websockify (pasar de WebSocket a TCP) son unas decenas de líneas de Go: los dos pueden ir en la CLI, no en el guest (ver [Decisión](#decisión)). El intérprete de Python se queda, porque lo pide Openbox.
- **Openbox cuesta 26 paquetes y 78 MiB**, casi todo por imlib2 (códecs de imagen, Ghostscript, librsvg). Un gestor de ventanas más pequeño, o ninguno con el navegador a pantalla completa, lo ahorraría.
- Mesa y LLVM no se pueden quitar: los piden Chromium y el servidor X a la vez.

### Arrancar

| | Normal (512 MiB) | Escritorio (2 GiB) |
|---|---|---|
| Hasta que corre un comando | 5,1 a 7,1 s, mediana **7,1 s** | 13,1 a 15,1 s, mediana **13,1 s** |
| Hasta que el navegador contesta por DevTools | — | 15,0 a 16,1 s, mediana **15,8 s** |
| Latencia de `exec` | 20 ms | 25 ms, con el navegador abierto |

**Dónde se van los 6 s de más.** En montar la raíz. El kernel del guest tarda 0,8 s desde que arranca hasta montar `/dev/vda` con la imagen normal, y **6,9 s** con la de escritorio: es la copia dispersa de 1,1 GiB que el nodo acaba de hacer para la sandbox, que el host todavía está escribiendo cuando el guest la lee. [ADR-0017](0017-fast-start.md) midió lo mismo con una copia de 310 MB (1,7 s). Con un overlay qcow2 ([#204](https://github.com/luisgf/agent-sandbox-platform/issues/204)) no hay nada que copiar ni que escribir. El banco de [ADR-0017](0017-fast-start.md) ([`scripts/bench-vm-start.py`](../../scripts/bench-vm-start.py)), en la misma VM anidada, desde que se lanza Cloud Hypervisor hasta que contesta el pod-daemon (medianas de 3):

| Disco | Normal | Escritorio |
|---|---|---|
| Una copia dispersa nueva, arrancada en el acto (lo que hace un nodo hoy) | 3,4 s | **11,2 s** |
| La copia, vaciada a disco antes de arrancar | 3,4 s (0,9 s de vaciado) | 11,0 s (8,0 s de vaciado) |
| Un overlay qcow2 sobre la imagen, sin copia | 2,8 s | **3,1 s** |
| Lo mismo, con un workspace | 3,3 s | 3,8 s |

Con el overlay, la imagen de escritorio arranca a 0,3 s de la normal, y el overlay ocupa 1,1 MiB después del arranque, frente a los 1,1 GiB de la copia.

Después de montar la raíz, el resto es el mismo que con la imagen normal más el propio navegador: el pod-daemon contesta 0,8 s después, y Chromium, con 1 vCPU, 2 a 3 s más tarde.

### Memoria

Una sandbox con 1 vCPU, que arranca, carga un artículo de Wikipedia y espera 5 s:

| Memoria del guest | La primera carga | Usado en el guest | Lo que ocupa la VM en el host |
|---|---|---|---|
| 512 MiB | **no carga**: el navegador contesta a `/json/version`, pero no a la primera orden en 60 s | 208 MiB | 526 MiB |
| 768 MiB | 15,5 s | 418 MiB | 754 MiB |
| 1 GiB | 4,5 s | 456 MiB | 985 MiB |
| 1,5 GiB | 10,0 s ¹ | 489 MiB | 1092 MiB |
| 2 GiB | 2,9 s | 482 MiB | 1090 MiB |

¹ La primera carga pasa por tres proxies hasta Internet y varía mucho; la tabla de páginas de abajo es la que mide el navegador.

- **Con menos de 1 GiB el navegador se arrastra o no llega a servir**: no hay swap y el guest se queda sin caché para el propio Chromium. 1 GiB es el mínimo y 1,5 GiB, el tamaño cómodo.
- **El host paga casi toda la memoria que el guest toca**, y el guest toca unos 1,1 GiB: medio GiB de procesos y el resto, caché de páginas (Chromium y sus bibliotecas, leídos al arrancar). Cloud Hypervisor no recupera esa caché: no hay balloon ni free page reporting configurados. Por eso una sandbox de 1,5 o 2 GiB ocupa lo mismo en el host, y una de 1 GiB, poco menos.
- **La pantalla es barata.** El mismo navegador con `--headless=new` y sin Xvnc, Openbox ni noVNC deja 408 MiB usados en el guest, frente a 481 MiB con pantalla: unos 73 MiB. Lo caro es Chromium, no verlo.
- Los procesos, a 2 GiB: Chromium son 9 procesos de 49 a 222 MiB de residente cada uno (con páginas compartidas contadas varias veces), Xvnc 79 MiB y websockify 36 MiB.

### Páginas

En una sandbox de 1,5 GiB con el navegador ya caliente, tres cargas de cada:

| | Cargas | Mediana |
|---|---|---|
| `https://example.com/` | 3022, 71 y 60 ms | 71 ms |
| Un artículo de Wikipedia (13 a 19 imágenes) | 790, 4888 y 261 ms | 790 ms |

Pasan por el proxy de ASP, el reenviador del laboratorio y su proxy: la red domina, no la sandbox.

### Egress

| Prueba | Resultado |
|---|---|
| El navegador abre un sitio permitido | carga |
| El navegador abre `https://www.google.com/` (no permitido) | `ERR_TUNNEL_CONNECTION_FAILED`; el proxy contesta 403 y escribe la línea de auditoría con el `sandbox_id` |
| El navegador abre `http://neverssl.com/` (no permitido) | Chromium prueba antes HTTPS (443) y luego HTTP (80): los dos, denegados y auditados; se ve la página del 403 |
| `getent hosts` de un nombre permitido / no permitido | resuelve / NXDOMAIN (el sumidero de DNS) |
| TLS directo a `1.1.1.1:443`, sin pasar por el proxy | falla: el nodo lo redirige al proxy, que no habla TLS sin `CONNECT` (`WRONG_VERSION_NUMBER`) |
| UDP a `1.1.1.1:443` (QUIC) | sin respuesta: el nodo lo descarta |
| DNS por UDP a `8.8.8.8` | lo contesta el sumidero del nodo: NXDOMAIN |
| TCP a otro puerto (`1.1.1.1:22`) | bloqueado |
| Que el proxy llegue a una dirección del propio nodo | rechazado (`destination address not allowed: … is an address of this node`): el laboratorio lo encontró sin querer, al poner la dirección falsa en el loopback de la VM |

**El tráfico de fondo.** En 120 s sin usarlo, Chromium intentó 15 conexiones a seis hosts de Google (`accounts.google.com` seis veces, `android.clients.google.com`, `update.googleapis.com`, `clients2.google.com`, `redirector.gvt1.com` y `www.google.com`). Sin la política gestionada, 13 a los mismos hosts más `optimizationguide-pa.googleapis.com`. **La política apenas lo reduce.** Con los flags que pone Playwright contra el tráfico de fondo (`--disable-background-networking`, `--disable-component-update`, `--disable-sync` y otros) baja a 9 conexiones a tres hosts (`accounts.google.com`, `android.clients.google.com` y `update.googleapis.com`), y la navegación sigue funcionando. El proxy lo deniega todo, pero cada navegador abierto escribe unas siete líneas de auditoría por minuto. Y la barra de direcciones busca en Google por defecto: escribir algo que no es una dirección acaba en una página denegada.

### Acceso

- **DevTools por vsock.** Desde el nodo, un puente de 200 líneas (escucha en TCP, abre el socket vsock de Cloud Hypervisor de la sandbox y le dice `CONNECT 9222`) basta para que un cliente de DevTools fuera del guest cargue el artículo y haga la captura: 2,0 s en total. Ningún puerto de red del guest se abre: es el mismo camino por el que el nodo llega al pod-daemon (26500).
- **noVNC por vsock.** El mismo puente con el puerto 6080 sirve noVNC. Con un túnel SSH desde un portátil hasta ese puente (lo que haría un `asp session forward`), el escritorio de la sandbox se ve y se maneja desde un navegador cualquiera: ratón y teclado llegan.
- **Lo que no sirve hoy.** ASP no tiene reenvío de puertos, y `asp session exec` no es un canal de bytes (eventos NDJSON con el texto en UTF-8 con reemplazo), así que no puede llevar ni VNC ni WebSocket. [ADR-0010](0010-on-demand-local-net.md) va en el otro sentido (el guest sale hacia el portátil) y el nodo descarta todo lo que entra hacia un guest.

### Parar y reanudar

| | Escritorio (1,5 GiB) |
|---|---|
| Parar | 5,0 s |
| Reanudar hasta que el navegador contesta | 7,5 s |
| Qué sobrevive | el perfil (historial, cookies, sesiones iniciadas), que está en el disco; las pestañas abiertas, no |

### Densidad

Ocho sandboxes de escritorio pedidas a la vez, cada una carga el artículo cuando su navegador contesta:

| | 8 de 1 GiB | 8 de 1,5 GiB |
|---|---|---|
| Hasta que las 8 tienen el navegador listo | 83 s | 81 s |
| Páginas cargadas | 8 de 8 | 8 de 8 |
| Memoria del host por sandbox | 984 MiB | 1083 MiB |
| Disco del host por sandbox | 1125 MiB | 1114 MiB |

- **Llegan por tandas**: las primeras contestan a los 40 s y las últimas a los 75 a 79 s. Lo que se reparten es el disco: ocho copias de 1,1 GiB a la vez. Con la imagen normal, ocho de 512 MiB a la vez ejecutan su primer comando a los 19,1 s en la misma VM; las de escritorio, a los 75 a 79 s.
- **Con la espera por defecto de la CLI (60 s) la mitad no llega**: `asp session start` se rinde, borra la sandbox y no deja sesión. Con la imagen de escritorio, varias a la vez necesitan `--timeout` largo, o el overlay.
- **Cuántas admite el planificador.** No sobresuscribe la memoria y cuenta la pedida, no la usada: en el host de pruebas (31,1 GiB y 12 hilos) admite 17 sandboxes de escritorio de 1,5 GiB o 26 de 1 GiB, frente a 48 normales de 512 MiB (ahí manda la CPU: 12 hilos sobresuscritos 4 veces). Las 17 de 1,5 GiB ocupan de verdad unos 18 GiB; las 26 de 1 GiB, unos 25 GiB, que en un host que además sirve otras cosas ya es demasiado: el límite útil es la memoria real, no la que admite el planificador.

## Lo que dicen los números

1. **Se puede, y sin tocar ASP.** Un navegador con pantalla cabe en una sandbox como una imagen distinta: el egress, la identidad, el workspace, parar y reanudar funcionan igual. El navegador es un cliente más del proxy, y nada de lo probado sale por fuera.
2. **Lo caro es la copia del disco, no la pantalla.** La imagen ocupa 1,1 GiB en el host por sandbox y retrasa el arranque 6 s, porque el nodo la copia entera. Ocho a la vez tardan 80 s en tener el navegador listo; ocho normales, 19 s en ejecutar su primer comando, en la misma VM. Es exactamente lo que quita el overlay qcow2 de [#204](https://github.com/luisgf/agent-sandbox-platform/issues/204), que con la imagen normal ahorraba 1,7 s y aquí ahorra la mayor parte del retraso.
3. **La memoria manda en cuántas caben.** Hace falta 1 GiB de guest como mínimo, y el host paga ~1,1 GiB por sandbox con el navegador abierto, porque la caché de páginas del guest nunca vuelve. En el host de pruebas el planificador admite 17 sandboxes de escritorio de 1,5 GiB, frente a 48 normales de 512 MiB.
4. **Mirar es barato y llegar es lo que falta.** La pantalla cuesta 73 MiB. Lo que no existe es el camino de una persona hasta ella: ASP no tiene reenvío de puertos, y el vsock (el mismo canal que el nodo ya usa para el pod-daemon) basta para construirlo sin abrir puertos.

## Decisión

**Propuesta: el escritorio entra como una variante opcional de la imagen, pero no se ofrece hasta que existan tres cosas.**

- **La variante.** `ASP_GUEST_VARIANT=desktop` construye la imagen normal más Xvnc, Openbox, Chromium y noVNC, reproducible como la otra (tres builds del mismo commit, dos de ellos desde cero, dieron los mismos bytes), y se publica junto a ella en cada release. La imagen normal no cambia ni engorda.
- **Antes de ofrecerla, en este orden:**
  1. **Discos como overlay qcow2** ([#204](https://github.com/luisgf/agent-sandbox-platform/issues/204)). Sin ellos cada sandbox de escritorio copia 1,1 GiB y arranca 6 s más tarde, y varias a la vez se estorban.
  2. **Elegir la imagen por sandbox.** Hoy un nodo arranca una sola imagen (`/opt/sandbox`), y `image_ref` solo se guarda. El nodo tendría varios juegos instalados (`asp image pull --version <v>-desktop`) y la sandbox diría cuál (`asp session start --image desktop`); el planificador solo pondría una sandbox en un nodo que tenga su imagen.
  3. **Un reenvío de puertos autenticado por vsock**: `asp session forward <puerto>` y, encima, `asp session desktop`. El nodo llega al puerto vsock del guest como llega al pod-daemon; el plano de control lo autoriza como un `exec` (la dueña de la sandbox, o `sandbox:exec-any`); la CLI escucha solo en el loopback del portátil. El guest no abre puertos de red y el nodo no acepta tráfico entrante: [ADR-0010](0010-on-demand-local-net.md) sigue igual. **noVNC va en la CLI**, no en el guest: la CLI sirve sus ficheros estáticos y traduce WebSocket a VNC sobre el reenvío, y el guest solo expone VNC y DevTools por vsock. Eso quita de la imagen 73 paquetes y 128 MiB (Node.js, NumPy, websockify y lo que arrastran).
- **El tamaño.** Una sandbox de escritorio pide 1,5 GiB (el mínimo útil es 1 GiB; 512 MiB no sirve). La CLI lo pone por defecto con `--image desktop`.
- **El egress no cambia.** La política gestionada del navegador y los flags contra el tráfico de fondo son higiene (menos líneas de auditoría), no la frontera: la frontera es el nodo, y las pruebas lo confirman. Quedan pendientes, antes de publicarla, dos cambios que la variante medida no tiene: arrancar el navegador con los flags de Playwright (de 15 a 9 conexiones de fondo en dos minutos) y añadir a su política `DefaultSearchProviderEnabled: false`, para que la barra de direcciones no busque en un sitio denegado.

**Fuera de esta decisión:** un navegador sin pantalla como imagen aparte (la pantalla cuesta 73 MiB: no compensa otra imagen que mantener), recuperar la memoria del guest con balloon o free page reporting (mejoraría la densidad de todas las sandboxes, no solo estas; merece su propia medida), y grabar o compartir la sesión de escritorio.

## Alternativas

| Opción | A favor | En contra | Veredicto |
|---|---|---|---|
| **(A)** Variante opcional de la imagen, con overlay, imagen por sandbox y reenvío por vsock | funciona hoy sin tocar ASP; la imagen normal no cambia; el egress y la identidad, iguales | tres piezas antes de ofrecerla; otra imagen que construir y publicar | **Propuesta** |
| **(B)** Meterlo en la imagen normal | una sola imagen | multiplica por 8 la imagen de todas las sandboxes y les añade 6 s de arranque, la usen o no | rechazada |
| **(C)** Instalar el navegador al vuelo dentro de la sandbox | ninguna imagen nueva | el guest no tiene DNS ni apt por diseño; ~1 GB por sandbox cada vez | rechazada |
| **(D)** Solo un navegador sin pantalla, con DevTools | un poco menos de memoria | ahorra 73 MiB y deja a la persona sin ver nada; la imagen apenas adelgaza (Chromium arrastra GTK igual) | rechazada como imagen aparte; se puede arrancar así dentro de la variante |
| **(E)** Una pantalla de verdad (virtio-gpu) | aceleración, más resolución | Cloud Hypervisor no la tiene; el parche de Spectrum es de fuera del árbol | rechazada |
| **(F)** Wayland (cage o sway sin monitor, y wayvnc) | más moderno | menos probado; un fallo conocido de pantalla gris con wayvnc sin monitor | rechazada por ahora |
| **(G)** KasmVNC o Selkies | mejor imagen, WebRTC | fuera del archivo de Debian o más pesado; para una ventana no hace falta | rechazada |
| **(H)** El navegador fuera de la sandbox (un servicio como Browserbase, o un contenedor en el host) | nada que meter en la imagen | sale del egress y de la identidad de la sandbox: justo lo que ASP aporta | rechazada |

## Consecuencias

- Una imagen más en cada release: `asp-guest_<v>-desktop_*` (380 MiB comprimida). El CI la construiría en `release.yml`; en cada PR, construirla dos veces para comprobar que es reproducible añade unos minutos y depende de `snapshot.debian.org`, así que iría solo en la release y en un workflow manual.
- Chromium sigue a la rama de seguridad de Debian 12 (`bookworm-security` lo tenía al día en la foto del archivo). Si Debian deja de actualizarlo en oldstable, la variante pasa a Debian 13 antes que la imagen normal, o toma el Chromium de otro sitio con su hash fijado.
- El reenvío de puertos sirve para más que el escritorio (un servidor de desarrollo en la sandbox, por ejemplo), y es la primera vía de entrada hacia un guest: necesita su propio ADR.

## Límites honestos

- Todo se midió con virtualización anidada, en un host y en un día: los tiempos absolutos no son los de producción; la diferencia entre las dos imágenes sí vale.
- Las páginas cargaban a través de tres proxies hasta Internet: los tiempos de carga miden sobre todo esa red.
- Un artículo de Wikipedia no es una aplicación web pesada: con una SPA grande o varias pestañas, 1,5 GiB puede quedarse corto.
- Que la variante es reproducible se comprobó en un host y un día: tres builds del mismo commit, dos desde cero, con los mismos bytes. El CI todavía no la construye.
- No se probó WebRTC con un servidor STUN alcanzable (el nodo descarta UDP de todos modos), ni descargas de ficheros, ni audio.
- El teclado por noVNC se probó con un navegador automatizado, que traducía mal las teclas modificadoras y la puntuación; con un teclado de verdad no debería pasar, pero no se probó.

## Cómo repetirlo

Los scripts están en [`scripts/spike-browser/`](../../scripts/spike-browser/README.md): la VM desechable, la salida del proxy, las medidas, el cliente de DevTools y el puente vsock. La variante se construye con `ASP_GUEST_VARIANT=desktop scripts/build-guest-image.sh`.
