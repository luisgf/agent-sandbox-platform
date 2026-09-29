# CLI `asp`

Cliente HTTP de demostración para el control-plane: ciclo de vida de sandboxes.

```bash
# Desde la raíz del repo
make asp
./build/asp sandbox run --node-id=dev-node --cmd 'echo hello'
```

Subcomandos: `create`, `get`, `list`, `exec`, `delete`, `run`.

Documentación: [`docs/why-cli-asp.md`](../docs/why-cli-asp.md), quickstart en [`docs/mvp-smoke.md`](../docs/mvp-smoke.md) §8.
