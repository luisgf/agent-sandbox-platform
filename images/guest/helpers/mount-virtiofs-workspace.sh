#!/bin/sh
# Monta el tag virtiofs "workspace" en /workspace al arrancar el guest.
#
# El host solo expone el tag cuando workspace_host_path no está vacío
# (virtiofsd + fs en vm.create). Un sandbox sin workspace no tiene el
# dispositivo: este script sale 0 igual, para no frenar el boot.
#
# Sin dispositivo virtio-fs no hay nada que esperar: sale en el acto. Esperar 8 intentos de
# 0,25 s a un tag que no existe añadía unos 1,8 s al arranque de toda sandbox sin workspace
# (medido en un host KVM: 3,2 s con el helper de antes, 1,3 s con éste; ADR-0017). Con el
# dispositivo presente sigue reintentando, porque el driver puede tardar en cargarse.
#
# Variables (tests / drop-in):
#   ASP_WORKSPACE_MOUNT   punto de montaje (default /workspace)
#   ASP_VIRTIOFS_TAG      tag (default workspace)
#   ASP_VIRTIOFS_MOUNT_TRIES  intentos (default 8)
#   ASP_VIRTIOFS_MOUNT_PAUSE  pausa entre intentos, en segundos (default 0.25)
#   ASP_VIRTIO_SYSFS      dónde mirar los dispositivos virtio (default /sys/bus/virtio/devices)
set -u

dest="${ASP_WORKSPACE_MOUNT:-/workspace}"
tag="${ASP_VIRTIOFS_TAG:-workspace}"
tries="${ASP_VIRTIOFS_MOUNT_TRIES:-8}"
pause="${ASP_VIRTIOFS_MOUNT_PAUSE:-0.25}"
sysfs="${ASP_VIRTIO_SYSFS:-/sys/bus/virtio/devices}"

# virtio-fs es el dispositivo virtio número 26 (0x001a).
has_virtiofs_device() {
  for dev in "$sysfs"/*; do
    [ "$(cat "$dev/device" 2>/dev/null)" = "0x001a" ] && return 0
  done
  return 1
}

mkdir -p "$dest" || {
  echo "workspace-virtiofs: no se pudo crear $dest; el boot sigue" >&2
  exit 0
}

if command -v mountpoint >/dev/null 2>&1 && mountpoint -q "$dest" 2>/dev/null; then
  exit 0
fi

# Los dispositivos virtio existen desde que el kernel enumera el bus PCI, antes de que se cargue
# ningún driver: si ya no hay uno de virtio-fs, no va a aparecer.
if ! has_virtiofs_device; then
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
