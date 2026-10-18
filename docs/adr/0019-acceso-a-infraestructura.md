# ADR-0019: Acceso a infraestructura desde la sandbox: un broker fuera del guest (infrabroker)

- **Estado:** Aceptada
- **Fecha:** 2026-10-18
- **Implementación:** la Fase 0 no necesita código en ASP (es un montaje: [usar ASP con infrabroker](../how-to/infrabroker.md)). La Fase 1 toca infrabroker (un claim de contexto hasta el certificado y su audit), no ASP. Nada de esto está probado en un host con KVM todavía.
- **Relacionados:** [0002](0002-networking.md) (el egress solo deja HTTP(S) y DNS), [0003](0003-identity.md) (el puente del agente SSH y los tokens OIDC), [0007](0007-multi-user-identity.md) (`user_sub` en el token de workload), [0010](0010-on-demand-local-net.md) (con `--local-net` no hay allowlist), [0013](0013-kubernetes-reevaluation.md) (el público de ASP), [0018](0018-browser-desktop.md) (un agente que corre dentro del guest), [el modelo de seguridad](../concepts/security-model.md), [infrabroker](https://github.com/luisgf/infrabroker) y su [`docs/SANDBOX.md`](https://github.com/luisgf/infrabroker/blob/main/docs/SANDBOX.md)
- **No es:** un cliente MCP en la imagen del guest, un vault de secretos dentro del guest, ni un cambio en el egress de [0002](0002-networking.md).

## Contexto

ASP responde a **dónde corre el código del agente** y a qué red llega: una microVM por sesión, egress denegado por defecto, y las credenciales del operador fuera del guest ([0003](0003-identity.md)). No responde a cómo ese agente **actúa sobre infraestructura**: un host de producción, un bastión, un cluster. [0003](0003-identity.md) deja escrito que «no hay vault de secretos genérico dentro del guest; solo SSH bridge + OIDC corto», y lo único que se parece a una política por acción es la confirmación de cada firma SSH ([0005](0005-fase-2d-hardening.md)).

Lo que sí hay es un camino, y es el equivocado. El puente del agente SSH por vsock (26501) es un **oráculo de firma** con las claves del operador: el guest no ve la clave, pero puede pedir firmas con ella, y [0003](0003-identity.md) cita «bastion» entre sus casos de uso. Fuera de multiusuario la confirmación por firma es opcional. Y con `--local-net` arriba ([0010](0010-on-demand-local-net.md)) la sesión saca **todo** su unicast por el portátil del usuario, sin filtro de puertos y sin la allowlist: un guest hostil con el puente llega a cualquier host de la LAN cuyo `authorized_keys` confíe en esa clave. Es exactamente el patrón de «clave reutilizable al alcance del agente» que una herramienta de acceso para agentes existe para quitar de en medio.

[infrabroker](https://github.com/luisgf/infrabroker) es esa herramienta, del mismo autor y para el mismo público que [0013](0013-kubernetes-reevaluation.md) fija para ASP (quien se autoaloja, de uno a tres hosts): el modelo pide una *acción*; un signer aparte la comprueba contra una política por comando (allow, deny, `require_approval` con aprobación humana), acuña un certificado SSH de minutos con `force-command` y `source-address`, el broker ejecuta y el modelo solo recibe `stdout`, `stderr` y el código de salida. Audit encadenado y firmado, correlado con el `sshd` del host por `serial`. Y declara como fuera de alcance justo lo que ASP cubre: dónde corre el modelo (en su modo stdio «el aislamiento es el proceso del SO», y el certificado mTLS del broker vive en esa misma máquina).

Dos hechos del código de hoy hacen que los dos encajen sin tocarse:

1. **En modo `enforce` el guest no puede hacer SSH a nada.** El egress solo deja HTTP(S) y DNS hacia la allowlist del tenant ([modelo de seguridad](../concepts/security-model.md#el-guest-frente-a-la-red)). `infrabroker serve-mcp-http` es HTTPS: cabe en la allowlist. La conexión SSH **nace en el broker, fuera de la sandbox**. El límite conocido «el egress solo filtra HTTP(S)» se cierra desde el otro lado: nadie necesita :22.
2. **El token de workload ya es una identidad que infrabroker entiende.** El guest pide un token por vsock 26502 eligiendo solo `aud`; el plano de control pone `sub=sandbox/{id}`, `tenant_id`, `sandbox_id` y `user_sub` (el humano dueño, [0007](0007-multi-user-identity.md)) y publica discovery y JWKS. El frontend HTTP de infrabroker valida un bearer contra cualquier issuer OIDC, con el claim de usuario configurable (`user_claim`), y su signer puede revalidarlo dentro de su frontera de confianza (`require_verified_end_user`). `user_claim: "user_sub"` atribuye cada acción del broker al humano dueño de la sandbox.

Ninguno de los dos repositorios nombraba al otro.

## Decisión

### 1. Reparto de papeles: el puente SSH firma para git; la infraestructura va por infrabroker

| Camino | Para qué | Para qué **no** |
|---|---|---|
| Puente del agente SSH por vsock ([0003](0003-identity.md)) | Control de versiones: `git fetch`, `git push`, deploy keys | Entrar en hosts: no hay política por comando, no hay aprobación por acción, y la clave que firma sirve para todo lo que confíe en ella |
| infrabroker (HTTPS, por la allowlist del tenant) | Hosts y clusters: una acción, un certificado de minutos, política, aprobación, audit | — |

[0003](0003-identity.md) queda enmendado: «bastion» deja de ser un caso de uso del puente. La recomendación operativa: con `--local-net`, la confirmación por firma encendida (`--ssh-agent-confirm`, que ya es el valor por defecto en multiusuario) o el puente apagado para esa sesión.

### 2. El broker vive en el host de infrabroker, no en el nodo

El proxy de ASP **nunca** marca loopback, direcciones del nodo (alias incluidos) ni la red de los guests ([modelo de seguridad](../concepts/security-model.md#el-guest-frente-a-la-red)). Un `serve-mcp-http` en la IP del nodo no es alcanzable desde una sandbox, y no se abre una excepción para ello. El broker vive en **otra máquina**, la que infrabroker ya recomienda para su signer, y su nombre entra en la allowlist del tenant como cualquier otro destino.

No es solo que cueste cero código. El nodo es la máquina más privilegiada del sistema (root, el VMM, los discos de todas las sandboxes) y es donde corre el código hostil; el certificado mTLS con que el broker habla con el signer no tiene por qué estar ahí. La frase que resume la pareja es «la máquina que ejecuta el código del agente y la que custodia las claves son distintas». De paso, el `source-address` de cada certificado se fija a la IP estable del host del broker, que es lo que infrabroker ya hace sobre una malla.

### 3. La identidad cruza por el token de workload

Cuando el runtime del agente corre dentro del guest (el caso de [0018](0018-browser-desktop.md)), pide un token con `aud=infrabroker` al socket de identidad y lo presenta como bearer a `serve-mcp-http`. infrabroker se configura con el plano de control de ASP como issuer, `user_claim: "user_sub"` y, cuando exista (Fase 1), un claim de contexto `sandbox_id` que el signer sella en el `KeyID` del certificado y en su audit. Así el audit de infrabroker (por `serial`) y el de ASP (por `sandbox_id`) se unen por `user_sub` + `sandbox_id`. ASP no cambia nada para esto: el token ya lleva esos claims.

### 4. Fases

| Fase | Qué | Código en ASP |
|---|---|---|
| **0** | El harness (OpenCode u otro) corre fuera del guest, como hoy; bash va a la sesión; infrabroker es un MCP stdio junto al harness. El modelo pide `ssh_execute`, el broker acuña y ejecuta, el guest nunca ve :22 | Ninguno: [usar ASP con infrabroker](../how-to/infrabroker.md) |
| **1** | El runtime del agente corre **dentro** de la sandbox y habla con `serve-mcp-http` con el token de workload; `sandbox_id` llega al certificado y al audit del broker | Ninguno (el claim de contexto es de infrabroker) |
| **2** | Un claim de grupos o roles en el token de workload, derivado del rol del dueño en el IdP, para RBAC por usuario en el signer; un cliente bearer mínimo en el guest para scripts; un lab de extremo a extremo | Sí, con su propio corte |

## Alternativas consideradas

| Alternativa | Pros | Contras | Decisión |
|---|---|---|---|
| **Seguir con el puente SSH para todo** (incluido «bastion») | Ya existe; una sola pieza | Clave reutilizable al alcance del guest; sin política por comando; con `--local-net`, la LAN entera | **Rechazada** para infraestructura. Se queda para git |
| **Un vault de secretos dentro del guest** | El agente usa sus herramientas de siempre | Un secreto en el guest es un secreto exfiltrable; contradice [0003](0003-identity.md) y la hoja de ruta («no guardar claves privadas en las imágenes») | **Rechazada** |
| **Un puente vsock nuevo (26503) del guest al broker** | Funciona en un solo host; funciona con `--local-net` | Código y un ADR propio en ASP (el primer servicio del host expuesto al guest, como el reenvío de puertos de #234); acopla los dos repositorios; pone el broker en el nodo | **Descartada.** Se reabre solo si «todo en una máquina» se vuelve un requisito del público de ASP |
| **Que el proxy marque una dirección del nodo para el broker** | Menos que un puente | Abre en el proxy justo lo que el modelo de seguridad promete que nunca marca | **Rechazada** |
| **Fundir los proyectos o compartir código** | Un solo relato | Licencias distintas (Apache-2.0 y GPL-3.0); dos públicos que ya valen por separado | **Rechazada.** La costura es protocolo: HTTPS, MCP, OIDC |

## Consecuencias

### Positivas

- Un hueco real de ASP (el puente como camino a la infraestructura) se convierte en una frontera dicha con claridad: el guest no puede hacer SSH a nada y no lo necesita.
- La Fase 0 funciona hoy, sin tocar código en ningún repositorio.
- El audit de cada acción sobre infraestructura lleva el humano dueño de la sandbox, sin que ASP acuñe nada nuevo.
- Cada proyecto sigue valiendo solo. La pareja es la arquitectura de referencia, no un requisito.

### Negativas / coste

- El broker es otra máquina (o la del signer). En un solo host estricto, la Fase 1 exige que el broker viva en algo que no sea la IP del nodo: otra caja, un contenedor con una IP enrutada propia, o el portátil del usuario. Si esa máquina tiene una dirección privada, el operador del nodo abre su rango con `--egress-allow-cidr` ([red y egress](../concepts/networking-and-egress.md)); la allowlist del tenant sola no basta.
- Con `--local-net`, la ruta por defecto de la sesión sale por el portátil: el nombre del broker tiene que resolverse y alcanzarse desde la red del usuario, no desde el nodo. Es la única ventaja real del puente vsock, y queda anotada.
- Los tokens de workload viven cinco minutos por defecto. El runtime del guest vuelve a pedir uno por llamada, y una aprobación humana en infrabroker que tarde más que la vida restante del token falla cerrada y se vuelve a pedir.

## Límites honestos

- **En la Fase 1 la autorización es por broker, no por sandbox.** Cualquier guest de ese plano de control puede pedir `aud=infrabroker` (el `aud` lo elige el guest), y sin un claim de grupos el RBAC por usuario del signer está apagado: todo dueño de sandbox alcanza lo que alcance el broker. El token da **atribución**, no alcance, hasta la Fase 2.
- Nada de esto está probado con KVM ni en un lab de dos máquinas. La Fase 0 se comprueba a mano ([usar ASP con infrabroker](../how-to/infrabroker.md#comprobarlo)).
- El guest no trae cliente MCP: el runtime que lo use lo trae él. ASP no promete que una imagen tenga uno.
- Kubernetes es ortogonal: [0013](0013-kubernetes-reevaluation.md) y las herramientas `k8s_*` de infrabroker no se tocan.

## Enmiendas

- **2026-10-18:** aceptada por el mantenedor tal como se propuso: el puente SSH firma para git, la infraestructura va por infrabroker, y el broker vive en el host de infrabroker y no en el nodo. La Fase 2 (un claim de grupos en el token de workload) empieza en su propio corte, con su ADR.

## Referencias

- [Usar ASP con infrabroker](../how-to/infrabroker.md): el montaje de la Fase 0 y lo que cambia en la Fase 1.
- infrabroker: [`docs/SANDBOX.md`](https://github.com/luisgf/infrabroker/blob/main/docs/SANDBOX.md) (la misma decisión vista desde el broker), [`docs/THREAT_MODEL.md`](https://github.com/luisgf/infrabroker/blob/main/docs/THREAT_MODEL.md).
- [0003](0003-identity.md) § Enmiendas, [modelo de seguridad](../concepts/security-model.md#el-guest-y-las-credenciales), [red local bajo demanda](../ops-local-net.md).
