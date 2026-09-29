# ADR-0001: VMM por defecto

- **Estado:** Accepted
- **Fecha:** 2026-09-29

## Contexto

La plataforma necesita una frontera más fuerte que un contenedor, arranque rápido, virtio, vsock y un camino razonable hacia ejecución local/desktop. También debe admitir un perfil de alta densidad en Linux.

## Decisión

**Cloud Hypervisor es el VMM por defecto.** Su modelo virtio-pci, soporte de vsock y arquitectura moderna ofrecen el mejor equilibrio para nodos de servidor y una evolución posterior hacia desktop.

**Firecracker será un perfil alternativo**, no el default, para despliegues Linux server-side de mayor densidad multi-tenant donde su modelo de dispositivos más reducido sea ventajoso.

## Consecuencias

- El node-agent implementará primero el API/CLI de Cloud Hypervisor y sus modelos de dispositivos.
- Las imágenes y metadatos evitarán acoplarse a un único VMM; la interfaz interna conservará start/stop/pause.
- Debemos fijar y validar versiones del VMM, kernel y virtio en CI.
- Cloud Hypervisor amplía la superficie de dispositivos frente a Firecracker; se minimizarán los dispositivos habilitados.
- El perfil Firecracker requerirá adaptación de configuración, pruebas y benchmarks propios; no se promete paridad inmediata.
- La abstracción de VMM no ocultará capacidades incompatibles: el scheduler usará perfiles explícitos.
