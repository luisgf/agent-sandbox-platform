# ADR-0003: Identidad dentro del guest

- **Estado:** Accepted
- **Fecha:** 2026-09-29

## Contexto

Los workloads necesitan firmar operaciones SSH y obtener identidad federada sin copiar claves privadas, refresh tokens ni atributos de tenancy al guest.

## Decisión

Se implementan dos mecanismos:

1. **SSH agent host-held:** el agente SSH y sus claves permanecen en el host. El node-agent lo proxifica por vsock y `pod-daemon` lo expone como socket Unix dentro del guest. Nunca se copia una clave privada.
2. **OIDC ligado a atestación:** tras validar la atestación del sandbox, el node-agent solicita/emite tokens de corta vida. El guest usa un socket Unix con `POST /v1/tokens/oidc` y sólo puede proporcionar `aud`. El guest no puede establecer `tenant_id`, `sandbox_id` ni otros claims de autoridad; el node-agent los deriva de la conexión y la atestación. El control plane publica JWKS para verificación y rotación.

## Consecuencias

- El proxy SSH debe limitar protocolo, conexión, tiempo y tasa; la política puede exigir confirmación para claves sensibles.
- El socket de identidad aplica permisos Unix y audiencia allowlisted, pero la autorización real permanece fuera del guest.
- Tokens OIDC tienen TTL corto, audiencia única, identificador `jti` y claims inmutables de tenant/sandbox.
- La indisponibilidad de atestación o JWKS falla cerrada; no hay token de respaldo persistente.
- Se necesita rotación de claves de firma con solapamiento de JWKS y auditoría de cada emisión.
- La confianza depende de medir imagen, kernel, configuración del VMM y asociación vsock correcta.

## Rotación de claves de firma (básica)

1. Generar nuevo PEM RSA (`openssl genrsa 2048`).
2. Desplegar con `ASP_OIDC_KEY=<nuevo>` y `ASP_OIDC_KEY_PREV=<anterior>`.
3. JWKS incluye ambas claves; mint firma solo con la actual (`kid` del current).
4. Tras expirar tokens viejos (TTL ≤ 5m por defecto) y refrescar cachés JWKS, retirar `ASP_OIDC_KEY_PREV`.
