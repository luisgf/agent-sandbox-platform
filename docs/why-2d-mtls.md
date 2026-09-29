# Por qué / Qué ganamos — Strict mTLS

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

VerifyClientCertIfGiven allows anonymous TLS hits on wrong routes.

## Qué ganamos

ASP_MTLS_STRICT=1 → RequireAndVerifyClientCert; enroll on plaintext localhost ASP_ENROLL_LISTEN.
