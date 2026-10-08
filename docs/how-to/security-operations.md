# Operaciones de seguridad

Lo que un operador hace para endurecer un despliegue y mantenerlo: la lista de comprobación de producción, rotar la clave OIDC y los certificados de nodo, mTLS estricto, la lista de imágenes permitidas de la atestación, el fencing de un nodo perdido y la confirmación de las firmas del agente SSH. El modelo (qué frontera protege qué): [architecture.md](../architecture.md) y los ADR 0003, 0005, 0007 y 0011.

## Endurecer un despliegue

| Control | Qué hacer |
|---|---|
| mTLS entre nodos y plano de control | `tls_cert`/`tls_key` + `client_ca` en el plano de control, `--mtls` en el nodo; el token de enroll compartido solo sirve para el primer alta de un nodo ([instalar el plano de control](install-control-plane.md)) |
| API keys | Siempre exigidas: sin ninguna ni IdP, el plano de control no arranca. `ASP_BOOTSTRAP_API_KEY` en un gestor de secretos y no en git, solo para crear las demás con `asp apikey create` y rotarla o revocarla después (`asp apikey rotate\|revoke`). `ASP_INSECURE_OPEN_API=1` solo en laboratorios |
| Identidad de las personas | Un IdP con audiencia y roles: [conectar un IdP](idp.md) |
| Auto-provision | **`ASP_AUTO_PROVISION=0`** (el valor por defecto): `1` deja las sandboxes `running` sin ninguna VM |
| Egress | `ASP_EGRESS_DEFAULT_ALLOW=0`, política por tenant, y el redirect de nftables en modo `enforce` (es el valor por defecto con el proxy): [red y egress](../concepts/networking-and-egress.md) |
| Secretos y claves | La CA, la clave OIDC y la de atestación en almacenamiento persistente y de modo `0600` (el modo producción no arranca con ellas en `/tmp`); rotación, abajo |
| Superficie de Cloud Hypervisor | El directorio de sockets `/run/asp` es de root y `0700` (el agente lo deja así); un socket por sandbox; cada VMM como un usuario sin privilegios ([cómo ejecuta un nodo las sandboxes](../concepts/node-runtime.md#el-vmm-sin-privilegios-un-usuario-por-vm)) |
| Guest | Imagen mínima, sin claves y sin `NET_ADMIN`. El `pod-daemon` corre como root dentro de la VM (lanza los comandos como otro usuario): la frontera es la VM |
| Observabilidad | Los eventos de `sandbox_events` y `node_events`; las métricas ([monitorización](monitoring.md)); no registrar cuerpos ni tokens |
| Virtualización anidada | Solo en laboratorio; en producción, hardware físico o KVM dedicado |

## Rotar la clave OIDC

El plano de control firma los tokens de workload con una clave RSA. Para cambiarla sin invalidar los tokens ya emitidos:

1. Genera nueva RSA PEM; configura `ASP_OIDC_KEY=/path/new.pem` y `ASP_OIDC_KEY_PREV=/path/old.pem`.
2. Reinicia control-plane: JWKS publica **ambas** (`kid` actual + previo); mint usa solo la actual.
3. Tras TTL de tokens antiguos (default 5m) + margen de caché JWKS de clientes, quita `ASP_OIDC_KEY_PREV` y reinicia.

## Rotar y revocar certificados de nodo

Un certificado de nodo robado no debe seguir hablando con el plano de control: se rota (se emite uno nuevo) o se revoca el nodo (se bloquea el certificado actual).

```bash
# Emitir cert nuevo (API key de plataforma o admin del IdP; el bootstrap token no sirve, solo enrola.
# Un nodo con mTLS se renueva solo con su certificado vigente)
curl -fsS -X POST -H "Authorization: Bearer $ASP_BOOTSTRAP_API_KEY" \
  https://cp.example:8443/v1/nodes/$NODE_ID/rotate-cert | jq .
# Instalar PEMs en --cert-dir del node-agent y reiniciar agente

# Revocar nodo (bloquea fingerprint actual): admin del IdP o API key de plataforma,
# no el bootstrap token, que tienen todos los nodos
curl -fsS -X POST -H "Authorization: Bearer $ASP_BOOTSTRAP_API_KEY" \
  https://cp.example:8443/v1/nodes/$NODE_ID/revoke
```

## mTLS estricto

Por defecto el listener TLS verifica el certificado de cliente **si lo hay** (el enroll no puede traerlo). Con mTLS estricto lo exige siempre, y el enroll pasa a otro listener en claro y en loopback:

```yaml
# /etc/asp/server.yaml.d/30-mtls.yaml
mtls_strict: true
enroll_listen: 127.0.0.1:8081      # solo enroll, con el token; el listener TLS principal exige certificado
```

Necesita `tls_cert`, `tls_key` y `client_ca` (sin TLS se ignora, con un aviso en el log). La rotación con un certificado vigente más una API key sigue por el listener TLS. Si alguien pone `enroll_listen` en `0.0.0.0` en una red hostil, el token de enroll es el único candado: protege el *bind*.

## Atestación remota

1. El control plane solo acepta claves que conoce; la que trae el bundle (`public_key_pem`) no vale. Con mTLS (`ASP_CLIENT_CA` y `https://`) el node-agent firma con la clave de su certificado de nodo y no hace falta nada más. Sin mTLS, comparte `ASP_ATTEST_KEY` (PEM ECDSA P-256) entre node-agent y control-plane, o da de alta la clave pública de cada nodo en `ASP_ATTEST_TRUSTED_PUBS`.
2. Tras `running`, el reconciler firma `BootStatement` y hace `POST /v1/sandboxes/{id}/attest`. El statement lleva el SHA-256 del kernel (`kernel_digest`), el de la imagen base de la que se copió el disco (`image_digest`), la versión del hipervisor y si es un arranque nuevo o un `resume`; los calcula el nodo ([ADR-0003](../adr/0003-identity.md)).
   - **Lista de imágenes permitidas:** `node-agent --print-measurement` (en el nodo, con el kernel y la imagen que usa) imprime una entrada `{name, kernel, rootfs, vmm}`; ponla en un fichero `{"images":[…]}` y apunta `ASP_ATTEST_ALLOWED_IMAGES` a él en el control plane. El fichero se relee al cambiar. Con lista, una evidencia de una imagen que no esté se rechaza (400, evento `sandbox.attestation_refused`) y no hay claim; sin lista se guardan los digests que declare el nodo, sin comprobarlos.
   - **Al reconstruir la imagen** cambia el digest: añade la entrada nueva antes de actualizar los nodos, y quita la vieja cuando no queden sandboxes de ella.
   - **Orden de actualización:** el control plane primero; un nodo nuevo firma campos que uno anterior no conoce y su firma no cuadra.
3. Consulta: `GET /v1/sandboxes/{id}/attestation`; verificación sin store: `POST /v1/attestation/verify`.
4. Mint OIDC incluye `x_asp_attestation` si la evidencia está dentro de `ASP_ATTEST_MAX_AGE` (default 10m): `measured`, `allowlisted`, `image_name`, `image_digest`, `kernel_digest`, `vmm_version` y `boot`, además del nodo y la clave que firmó. Con lista, sin entrada vigente no hay claim.
5. Hardware TPM/SEV: implementar la interfaz `Attestor` (plug-in futuro); el MVP es `SoftwareAttestor`.

## Fencing (STONITH)

Cuando el monitor da un nodo por perdido y tiene sandboxes, el plano de control llama a un *provider* para apagarlo antes de marcarlas `failed`:

| `ASP_FENCE_PROVIDER` | Comportamiento |
|---|---|
| `noop` / vacío | Sin fence (default) |
| `http_webhook` | `POST` JSON `{action:power_off,node_id}` a `nodes.fence_endpoint`; Bearer `fence_token` |
| `redfish` | Stub HTTP basic → `{endpoint}/redfish/v1/Systems/1/Actions/ComputerSystem.Reset` |
| `ipmi` | Exec `ipmitool … chassis power off` si existe; **SoftFail** si no |

El destino de cada nodo lo configura un admin (IdP admin o API key de plataforma), no el nodo:

```bash
asp node fence set node1 --endpoint https://bmc.example/redfish --token-env BMC_PW   # lee BMC_PW del entorno del CP al fencear
asp node fence set node1 --endpoint 10.0.0.9 --token-file /etc/asp/bmc.pw             # o de un fichero del host del CP
echo -n "$PW" | asp node fence set node1 --endpoint … --token-stdin                     # o se guarda en la base de datos
asp node fence clear node1
```

(API: `PUT`/`DELETE /v1/nodes/{id}/fence`.) El endpoint y el token nunca salen en ninguna respuesta; `GET /v1/nodes` solo dice `fence_configured`. Con `--token-env`/`--token-file` el secreto no llega a Postgres; un `ipmitool` recibe la contraseña por `IPMI_PASSWORD`, no por la línea de comandos. Los campos `fence_endpoint`/`fence_token` que mande un agente al registrarse se ignoran, y `ASP_FENCE_ENDPOINT`/`ASP_FENCE_TOKEN` del node-agent ya no hacen nada (avisa en el log). Cuando el monitor da un nodo por perdido y tiene sandboxes, el CP llama al provider (una vez por caída) antes de marcarlas `failed`. Si el fencing falla, se registra `node.fence_failed` y se marcan igual.

> **Ops:** STONITH real exige BMC out-of-band (Redfish/IPMI alcanzable aunque el host esté hung). Una fila en Postgres **no** apaga VMs huérfanas.

## Confirmación del agente SSH

En multi-user ([ADR-0007](../adr/0007-multi-user-identity.md), SSH por usuario): `ASP_SSH_AGENT_SOCK_TEMPLATE=/run/asp/ssh-agents/{owner_sub}.sock` + `ASP_MULTI_USER=1` (confirm default-on). Quien opera tiene que crear el socket con las claves del usuario (ASP no lanza ssh-agents); si la ruta no existe, la sandbox recibe un agente falso. Sin plantilla se usa el puente global (`--ssh-agent-bridge`), que no es seguro con varios usuarios.

```bash
node-agent ... --ssh-agent-confirm --host-vsock --reconcile
# Antes de que el guest de sb-1 firme (por su {vsock}_26501):
curl -fsS -X POST http://127.0.0.1:9100/v1/internal/ssh-agent/approve \
  -H "Authorization: Bearer $(sudo cat /var/lib/asp/agent.token)" \
  -d '{"ttl_seconds":60,"sandbox_id":"sb-1"}'
# Sin approve → SignRequest = SSH_AGENT_FAILURE
```

La aprobación solo vale para la sandbox que nombra. `--ssh-agent-bridge` y el listener host-vsock global no saben qué guest llama: con `--ssh-agent-confirm` deniegan toda firma, salvo `--insecure-ssh-agent-global-approvals` (lab). Por cualquier ruta, el guest solo puede listar claves y firmar: añadir, borrar o bloquear claves del agente del host devuelve `SSH_AGENT_FAILURE`.
