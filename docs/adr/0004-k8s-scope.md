# ADR-0004: Alcance de Kubernetes

- **Estado:** Accepted
- **Fecha:** 2026-09-29

## Contexto

Kubernetes puede simplificar el despliegue de servicios stateless, pero modelar cada sandbox como Pod mezcla dos schedulers y vuelve ambiguas la propiedad de dispositivos, red, estado y recuperación.

## Decisión

**Kubernetes es opcional y se limita al despliegue del control plane.** Los nodos de sandbox son bare metal o VMs con `node-agent`. **Los sandboxes no se programan como Kubernetes Pods.** El node-agent inicia y reconcilia microVMs directamente, manteniendo explícita la frontera de aislamiento.

## Consecuencias

- El scheduler del control plane gestiona capacidad de nodos y perfiles de VMM.
- Operar nodos requiere provisioning, upgrades, draining y observabilidad fuera del modelo Pod.
- No dependemos de runtimes, CNI ni device plugins para el ciclo de vida de cada sandbox.
- Un despliegue de control plane en Kubernetes debe seguir funcionando con nodos externos conectados por mTLS.
- No habrá modo “microVM dentro de Pod” en el MVP; cualquier revisión exige un ADR nuevo y threat model actualizado.
