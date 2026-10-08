# ADR-0014: Las VMs sobreviven al node-agent y el siguiente proceso las adopta

- **Estado:** Aceptada
- **Fecha:** 2026-10 (#111)
- **Contexto:** [ADR-0011](0011-multi-node.md) (nodos), [ADR-0012](0012-retained-disks.md) (discos retenidos)
- Nota: el número 0013 está reservado para la evaluación de Kubernetes/k3s (#143).

## Contexto

Cada sandbox es un `cloud-hypervisor` (y, con workspace, un `virtiofsd`) que el node-agent arrancaba como hijo suyo y mantenía en memoria. Reiniciar el agente —una actualización, un fallo, `systemctl restart`— mataba todas las VMs del nodo: el plano de control pasaba sus sandboxes a `stopped` (`node_agent_restarted`), con el disco intacto pero con los procesos del guest, los `exec` en curso y la memoria perdidos. Cada actualización del agente era una interrupción total. KubeVirt (un `virt-launcher` por VM), Kata (una shim por pod) y E2B (el orquestador separado de Firecracker) sobreviven a la caída de su demonio de nodo.

#105a ya pone cada VM en su propio servicio systemd (`asp-vm-<id>`, `asp-vm-<id>-fs`), con cgroup y límites, pero lo ataba al servicio del agente (`BindsTo=`) para que, al no adoptarse, nunca quedara una VM corriendo para nadie.

## Decisión

1. **Las VMs confinadas no se atan al agente.** Sin `BindsTo=`, un reinicio del agente no las toca (`--vm-survive-restart`, activo por defecto; `=false` restaura el atado). `KillMode=control-group` del agente alcanza solo a lo que cuelga del agente.
2. **Cada VM en marcha deja un registro** en `{--ch-socket-dir}/state/<id>.json` (`0600`, en `/run`): CID, TAP y /30, sockets (vsock, consola, workspace), disco y su digest base, el tenant y el `owner_sub`, la hora de arranque y la versión del registro. Se escribe cuando el guest contesta —nunca mientras arranca, así que un fallo a medias no deja nada que adoptar— y se borra al empezar a liberar la VM, antes que nada.
3. **Al arrancar, antes de registrarse**, el agente: (a) busca los registros cuya VM sigue viva —su servicio está activo y su API contesta `Running`/`Paused` (`vmm.Adopter.Alive`)—; (b) pasa esa lista a la limpieza del host, que **salva** sus procesos, sockets, TAP, túnel local-net y disco y borra todo lo demás (`--reap-only` hace lo mismo desde `ExecStopPost`: borra lo muerto, salva lo vivo); (c) adopta cada una: vuelve a vigilar el servicio (`unit.Adopted`, por sondeo de systemd, porque ya no es hijo de un `systemd-run --wait`), recupera el endpoint de exec, los aceptores guest→host, el prefijo del proxy de egress, el SSH scoped, su `virtiofsd` (`virtiofs.Adopt`) y aparta el CID y la /30 del reparto; (d) se registra con `adopted_sandboxes`.
4. **El plano de control no huérfana lo adoptado.** `RegisterNodeInput.AdoptedSandboxes` excluye esas sandboxes del paso a `stopped` que sigue a un cambio de `agent_instance_id`. Solo cuentan las de ese nodo; las demás, igual que antes. Un plano de control antiguo ignora el campo: el reinicio acaba como antes (las pasa a `stopped`, el agente las ve sin asignar y las para): seguro, solo menos útil.
5. **Un fallo posterior se informa igual.** Una VM adoptada cuyo servicio acaba se trata como cualquier VM que muere sola (ADR-0012 § 9, #118): `stopped` con `vmm_exited`. La causa exacta ya no está (el servicio, arrancado con `--collect`, desaparece al acabar y con él su resultado): el detalle lo dice.

## Alternativas

- **Reconectar por el socket sin tocar systemd.** El socket de la API de CH basta para hablar con la VM, pero no para saber que su proceso vive, pararla de forma fiable ni limpiar su cgroup. El servicio es la fuente de verdad del proceso.
- **Un helper (`asp-launcher`) por VM, como `virt-launcher`.** Más piezas por VM para lo que systemd ya da (cgroup, límites, journal, parada). Se reconsidera si se necesita un proceso de supervisión propio por VM.
- **Persistir el estado en el plano de control.** El estado es del host (sockets, TAP, CIDs): si el host se reinicia las VMs mueren y el registro en `/run` desaparece con ellas, que es lo correcto; guardarlo en el plano de control lo haría sobrevivir a la VM.
- **No adoptar nunca (lo anterior).** Cada actualización era una interrupción; se mantiene como opción (`--vm-survive-restart=false`) y sigue siendo lo que pasa sin confinamiento.

## Consecuencias

- Actualizar el agente deja de interrumpir las sandboxes. Un `exec` en curso sí se corta (el agente era el intermediario); la VM y sus procesos no.
- Parar el servicio `asp-node-agent` deja las VMs corriendo sin nadie que las gobierne: pasados `ASP_NODE_FAILOVER_AFTER` el plano de control declara perdido el nodo y falla sus sandboxes; al volver, el agente las ve sin asignar y las para. Para pararlas a mano: `systemctl stop asp-vms.slice`.
- Se pierden en un reinicio: el historial de la consola (el agente se reengancha), la política de egress hasta el primer sondeo y el plan local-net, que se reaplica en él.
- La primera actualización a una versión con esto todavía las detiene: las VMs que arrancó el agente anterior nacieron con `BindsTo=asp-node-agent.service` y sin registro. Se cordona y se drena esa vez; a partir de ahí no hace falta.
- Compatibilidad: el formato del registro lleva versión; uno de otra versión no se adopta (su VM se limpia y la sandbox queda `stopped`).
- Esto cambia lo que `--reap-only` y la limpieza de arranque hacen con una VM viva: ya no es un resto si tiene registro.
- Verificado en el laboratorio ([host de pruebas](../lab/README.md)), con una pila de prueba aparte de la que corre: `systemctl restart` y `kill -9` del agente con una VM con workspace en marcha (mismo pid de CH, `uptime` del guest continuo, el proceso en segundo plano sigue, `exec` y escritura en el workspace funcionan, la sandbox sigue `running` en el plano de control); una VM que muere con el agente parado se limpia y queda `stopped` y reanudable; `--reap-only` no toca una VM viva; parar y reanudar a través de la adopción; y `--vm-survive-restart=false` conserva el comportamiento anterior.
