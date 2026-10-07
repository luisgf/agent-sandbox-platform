# Por qué / Qué ganamos — SSH agent confirmation gate

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

El bridge SSH (`--ssh-agent-bridge` / `--host-vsock` :26501) convierte el guest en **cliente del agente SSH del host**. Eso es exactamente lo que queremos para git/deploy **sin copiar claves**.

Pero si el guest está comprometido, puede enviar `SSH2_AGENTC_SIGN_REQUEST` en bucle y firmar lo que quiera **sin que el operador se entere**. La capacidad de firma es tan sensible como la clave misma.

## Qué ganamos

- Flag **`--ssh-agent-confirm`** / `ASP_SSH_AGENT_CONFIRM=1`.
- Antes de cada SignRequest, el bridge exige un approve **one-shot** con TTL (default 30s):

```bash
curl -s -X POST http://127.0.0.1:9100/v1/internal/ssh-agent/approve \
  -H 'Content-Type: application/json' \
  -d '{"ttl_seconds":30,"sandbox_id":"sb-1"}'
```

- Sin approve vigente → respuesta `SSH_AGENT_FAILURE` (auto-deny).
- La aprobación es de **una sandbox**: solo la consume una firma que llegue por el acceptor host-vsock de `sb-1`. Sin `sandbox_id` → 400.
- Listar identidades **sigue** funcionando (el gate no rompe discovery). Añadir, borrar o bloquear claves no llega nunca al agente del host, con o sin gate.
- `--ssh-agent-bridge` y el listener host-vsock global no distinguen guests: con el gate, sus firmas se deniegan salvo `--insecure-ssh-agent-global-approvals` (lab), que acepta aprobaciones sin `sandbox_id` para esos listeners.

## Cómo encaja con 2e

El mount automático en guest (`vsock-ssh-agent-proxy`, ADR-0006) **no** desactiva el confirm gate. Al contrario: más fácil tener `SSH_AUTH_SOCK` en el guest → más importante el approve explícito en hosts sensibles.

## Límites honestos

- La aprobación es por sandbox, pero no por clave: no hay allowlist de fingerprints todavía.
- Automatizaciones que firman en bucle necesitan un supervisor que renueve approves, o desactivar el flag en lab.
- El endpoint de approve vive en el exec proxy localhost (`--agent-listen`); no lo expongas fuera del host.

## Multi-user (ADR-0007 fase 4)

Con `ASP_MULTI_USER=1` (antes también `ASP_IDP_REQUIRED=1`, que avisa) o `ASP_SSH_AGENT_SOCK_TEMPLATE`, el confirm gate queda **default-on**. El approve exige `sandbox_id` y acepta `actor_sub` / `X-ASP-Actor-Sub` para audit. El scoping de claves es por template de UDS (no por este gate solo): ver [`why-multi-user-identity.md`](why-multi-user-identity.md).
