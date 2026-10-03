#!/bin/sh
# Monta el tag virtiofs "workspace" en /workspace al arrancar el guest.
#
# El host solo expone el tag cuando workspace_host_path no está vacío
# (virtiofsd + fs en vm.create). Un sandbox sin workspace no tiene el
# dispositivo: este script sale 0 igual, para no frenar el boot.
#
# Variables (tests / drop-in):
#   ASP_WORKSPACE_MOUNT   punto de montaje (default /workspace)
#   ASP_VIRTIOFS_TAG      tag (default workspace)
#   ASP_VIRTIOFS_MOUNT_TRIES  intentos (default 8)
#   ASP_VIRTIOFS_MOUNT_PAUSE  pausa entre intentos, en segundos (default 0.25)
set -u

dest="${ASP_WORKSPACE_MOUNT:-/workspace}"
tag="${ASP_VIRTIOFS_TAG:-workspace}"
tries="${ASP_VIRTIOFS_MOUNT_TRIES:-8}"
pause="${ASP_VIRTIOFS_MOUNT_PAUSE:-0.25}"

mkdir -p "$dest" || {
  echo "workspace-virtiofs: no se pudo crear $dest; el boot sigue" >&2
  exit 0
}

if command -v mountpoint >/dev/null 2>&1 && mountpoint -q "$dest" 2>/dev/null; then
  exit 0
fi

i=0
while [ "$i" -lt "$tries" ]; do
  if mount -t virtiofs "$tag" "$dest"; then
    exit 0
  fi
  i=$((i + 1))
  if [ "$i" -lt "$tries" ]; then
    sleep "$pause" 2>/dev/null || true
  fi
done

echo "workspace-virtiofs: tag '$tag' no montado en $dest (ausente o no listo); el boot sigue" >&2
exit 0
