# Publicar una versión

Una versión es una etiqueta `vX.Y.Z`. Al empujarla, el workflow de release (`.github/workflows/release.yml`) ejecuta [goreleaser](https://goreleaser.com) con [`.goreleaser.yaml`](../../.goreleaser.yaml) y publica una GitHub release con todo lo que un nodo o un puesto de trabajo necesita sin compilar.

## Qué contiene una versión

| Artefacto | Qué es |
|---|---|
| `asp_<versión>_<os>_<arch>.tar.gz` | el CLI `asp` (linux y darwin, amd64 y arm64) |
| `asp-control-plane_<versión>_linux_<arch>.tar.gz` | el control plane, su unit de systemd y un `server.yaml` de ejemplo |
| `asp-node-agent_<versión>_linux_<arch>.tar.gz` | el node-agent, su unit y un `agent.yaml` de ejemplo |
| `asp-server_<versión>_linux_<arch>.tar.gz` | `asp-server` (todo en un host, [single-host.md](single-host.md)), su unit y un `standalone.yaml` de ejemplo |
| `*.deb`, `*.rpm` | los mismos cuatro como paquetes: `asp`, `asp-control-plane`, `asp-node-agent`, `asp-server` (éste depende de los otros tres). Dejan la unit en `/lib/systemd/system` y el fichero de ajustes en `/etc/asp/` (modo 0600, no se pisa al actualizar), y **no arrancan nada** |
| `*.sbom.json` | la lista de componentes de cada archivo |
| `asp_<versión>_source.tar.gz` | las fuentes |
| `SHA256SUMS` | las sumas de todo lo anterior: `sha256sum -c SHA256SUMS --ignore-missing` |
| `ghcr.io/luisgf/asp-control-plane:<versión>` | el control plane como imagen (amd64 y arm64, distroless, usuario no root) |

El kernel y la imagen del guest se publican aparte, con su propio `SHA256SUMS` (`scripts/build-guest-image.sh --verify --kernel`: el kernel se compila desde el fuente y la imagen se construye, cada uno dos veces desde cero, y el job falla si difieren; [`images/guest/README.md`](../../images/guest/README.md)).

Los binarios dicen de qué versión son: `asp version`, `asp-node-agent --version`, `asp-control-plane --version`. El agente la manda al registrarse y `asp node list` la muestra en la columna `VERSION`; las métricas `asp_build_info`, `asp_agent_build_info` y `asp_node_agent_info{node,version}` permiten seguir una actualización en un panel.

## Cómo se publica

1. **Prepara el `CHANGELOG.md`.** Pasa lo de `[Unreleased]` a una sección `## [X.Y.Z] - AAAA-MM-DD` y deja `[Unreleased]` vacío. Lo que rompe algo va en **Changed** o **Removed**, con lo que hay que hacer. Si es la primera versión, actualiza también «Supported versions» en [`SECURITY.md`](../../SECURITY.md).
2. **Comprueba el árbol:** `make lint test` y, si se tocó algo del datapath, `make smoke` (y la prueba con KVM: `make e2e-kvm`, [probar un nodo de punta a punta](e2e-kvm.md)).
3. **Mira lo que saldría** sin publicar nada: `make snapshot` deja todo en `dist/`. Prueba un paquete en un contenedor limpio: `docker run --rm -v $PWD/dist:/d debian:bookworm-slim sh -c 'apt-get update -qq && apt-get install -y /d/asp-node-agent_*_linux_amd64.deb && asp-node-agent --version'`.
4. **Etiqueta y empuja** desde `main`, con el commit del changelog ya dentro:

   ```sh
   git tag -s v0.1.0 -m "v0.1.0"      # -a si no firmas
   git push origin v0.1.0
   ```

5. **Verifica la release:** que el workflow termine, que `SHA256SUMS` valide los ficheros descargados, que `docker run ghcr.io/luisgf/asp-control-plane:0.1.0 --version` conteste (el paquete de ghcr se hace público una vez, en los ajustes de paquetes del repositorio) y que un nodo limpio instale el `.deb`.

Una versión con sufijo (`v0.2.0-rc.1`) se publica como pre-release.

## Versionado

[SemVer](https://semver.org/). Mientras la mayor sea 0, una menor puede cambiar ajustes o comportamiento, y el changelog lo dice. Una migración de base de datos (`control-plane/migrations`) solo añade: el control plane nuevo arranca sobre la base del anterior, y se actualiza **antes** que los nodos ([`ops-multi-node.md`](../ops-multi-node.md)).

## Subir Cloud Hypervisor

El instalador lleva fijada la versión de Cloud Hypervisor que pone en los nodos y la suma SHA-256 de sus binarios (`CH_VERSION`, `CH_SHA256_AMD64` y `CH_SHA256_ARM64`, al principio de `scripts/install.sh`): se niega a instalar un fichero con otra suma. Para subir la versión:

1. Pruébala en un host con KVM (`make e2e-kvm`): su API REST y sus opciones cambian entre mayores, y el cliente del nodo (`node-agent/internal/vmm`) las sigue.
2. Pon las sumas que GitHub publica para esos ficheros: `gh api repos/cloud-hypervisor/cloud-hypervisor/releases/tags/vX.Y --jq '.assets[] | select(.name|test("^cloud-hypervisor-static")) | [.name, .digest] | @tsv'`.
3. Cambia `TestedCloudHypervisor` en `node-agent/internal/doctor/checks.go` (el doctor avisa si la mayor instalada es otra) y la versión en [instalar un nodo](install-node.md#2-cloud-hypervisor). Un test (`TestTheInstallerPinsTheCloudHypervisorThatIsTested`) falla si el script y el doctor dicen mayores distintas.

## Primera vez en un repositorio

- El workflow vive en `.github/workflow-drafts/release.yml` hasta que quien empuja tenga el permiso `workflow` (`gh auth refresh -s workflow`): `git mv .github/workflow-drafts/release.yml .github/workflows/release.yml`.
- Firmar con cosign está apagado: el bloque `signs:` de `.goreleaser.yaml` está comentado y se enciende con firma sin clave (OIDC de GitHub) cuando se quiera.
