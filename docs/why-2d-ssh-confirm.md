# Por qué / Qué ganamos — SSH agent confirmation gate

Ver también ADR: [`adr/0005-fase-2d-hardening.md`](adr/0005-fase-2d-hardening.md).

## Por qué

compromised guest can silently sign via bridged agent.

## Qué ganamos

--ssh-agent-confirm + POST /v1/internal/ssh-agent/approve one-shot TTL; auto-deny SignRequest.
