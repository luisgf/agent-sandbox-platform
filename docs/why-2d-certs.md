# Por qué / Qué ganamos — Node cert rotation & revocation

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

La identidad de un nodo frente al control plane es un **client certificate** emitido en `POST /v1/nodes/enroll`. Sin ciclo de vida:

1. Un cert **robado** (backup filtrado, disco de lab reutilizado) sigue autenticando register/heartbeat/claim para siempre.
2. Un cert **caducado o reemplazado** en disco local puede divergir del fingerprint que el CP recuerda, sin forma limpia de rotar.
3. No hay forma de **echar** un nodo comprometido sin apagar el CP entero o regenerar la CA.

En un entorno corporativo FOSS esto es el primer hallazgo de cualquier revisión de mTLS “casero”.

## Qué ganamos

- **`POST /v1/nodes/{id}/rotate-cert`** — con un admin del IdP, una API key de plataforma o el bootstrap token (nunca una API key de tenant). Emite PEMs nuevos, guarda `cert_serial` + `cert_fingerprint` en `nodes`, y mete el fingerprint anterior en `node_cert_revocations`.
- **`POST /v1/nodes/{id}/revoke`** — admin del IdP o API key de plataforma; el bootstrap token no basta, porque lo tienen todos los nodos. Marca `nodes.revoked_at` y revoca el fingerprint actual. El nodo deja de poder hablar por mTLS hasta re-enroll.
- Middleware en `control-plane/internal/api/auth.go` rechaza fingerprints revocados (`client certificate revoked`).
- Migración **`006_node_cert_rotation.sql`**.

## Cómo se usa (ops)

```bash
# Rotar (nodo ya enrollado; API key de plataforma, admin del IdP o bootstrap token)
curl -s -X POST "https://cp/v1/nodes/node-a/rotate-cert" \
  -H "Authorization: Bearer $ASP_BOOTSTRAP_API_KEY" \
  -o /tmp/rotate.json
# Escribir PEMs nuevos en ASP_CERT_DIR del nodo y reiniciar node-agent

# Revocar (incidente)
curl -s -X POST "https://cp/v1/nodes/node-a/revoke" \
  -H "Authorization: Bearer $ASP_BOOTSTRAP_API_KEY"
```

## Límites honestos

- Revocar **exige re-enroll** para volver; no hay “unrevoke” silencioso del mismo cert.
- La CA de enrollment sigue siendo confianza raíz: proteger `ASP_CA_KEY` es ops, no magia de esta API.
- MemoryStore en lab también soporta el flujo, pero se pierde al reiniciar el API sin Postgres.
