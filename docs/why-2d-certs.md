# Por qué / Qué ganamos — Node cert rotation & revocation

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

stolen/expired node certs must not keep talking to CP forever.

## Qué ganamos

rotate-cert + revoke APIs, serial/fingerprint storage, mTLS middleware rejects revoked fingerprints.
