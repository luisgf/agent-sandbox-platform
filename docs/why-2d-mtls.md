# Por qué / Qué ganamos — Strict mTLS

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

Con `ASP_CLIENT_CA` el listener TLS del control plane usaba `VerifyClientCertIfGiven`:

- Si el peer **no** presenta client cert, la conexión TLS **sigue** (el cert es opcional a nivel Go `tls.Config`).
- La autorización real dependía del **middleware** HTTP. Una ruta nueva mal cableada (olvidar el wrapper, path público por error) quedaba **sondeable por clientes anónimos** sobre TLS.

Eso es aceptable en lab HTTP, inaceptable en un CP corporativo expuesto.

## Qué ganamos

- **`ASP_MTLS_STRICT=1`** → el listener TLS principal usa `RequireAndVerifyClientCert`. Sin client cert válido, ni siquiera hay HTTP.
- El enroll de nodos nuevos **no tiene** client cert aún → listener plaintext separado **`ASP_ENROLL_LISTEN`** (default `127.0.0.1:8081`) limitado a `/healthz` y `POST /v1/nodes/enroll`.
- Alternativa: rotar certs con un client cert vigente + API key **sobre** el listener estricto (sin abrir plaintext).
- Sin strict: se mantiene el comportamiento lab (`VerifyClientCertIfGiven` + middleware).

## Cómo se usa (ops)

```bash
export ASP_TLS_CERT=/var/lib/asp/certs/server.crt
export ASP_TLS_KEY=/var/lib/asp/certs/server.key
export ASP_CLIENT_CA=/var/lib/asp/certs/ca.crt
export ASP_MTLS_STRICT=1
export ASP_ENROLL_LISTEN=127.0.0.1:8081   # solo loopback / red de gestión
(cd control-plane && go run ./cmd/api)
```

El node-agent hace el enroll inicial contra el enroll listener (o vía túnel/ops) y después habla mTLS al listener principal.

## Límites honestos

- Si alguien pone `ASP_ENROLL_LISTEN=0.0.0.0:8081` en una red hostil, el bootstrap token es el único candado — **protége el bind**.
- Strict sin `ASP_TLS_CERT`/`ASP_TLS_KEY` se ignora (warning en logs).
- `/healthz` en el enroll listener es a propósito para probes locales; no expone datos de tenant.
