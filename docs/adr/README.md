# Decisiones de arquitectura (ADR)

Cada ADR recoge una decisión que costó tomar: el contexto, lo que se decidió, las alternativas que se descartaron y lo que cuesta. Los números no se reutilizan, y los ficheros conservan el nombre con que nacieron aunque el título haya cambiado (un ADR de «Fase 2d» sigue en `0005-fase-2d-hardening.md`).

| ADR | Decisión | Estado |
|---|---|---|
| [0001](0001-vmm-choice.md) | VMM por defecto: Cloud Hypervisor | Aceptada |
| [0002](0002-networking.md) | Red y control de egress: un TAP y una /30 por sandbox, proxy y sumidero de DNS en el host | Aceptada |
| [0003](0003-identity.md) | Identidad dentro del guest: agente SSH del host y tokens OIDC cortos, por vsock | Aceptada |
| [0004](0004-k8s-scope.md) | Alcance de Kubernetes: el plano de control puede ir en él, los nodos no | Aceptada |
| [0005](0005-fase-2d-hardening.md) | Rotación de certificados de nodo, mTLS estricto y confirmación de firmas SSH | Aceptada |
| [0006](0006-fase-2e-nft-ssh-guest.md) | Redirect de nftables completo y agente SSH automático en el guest | Aceptada |
| [0007](0007-multi-user-identity.md) | Identidad multi-usuario: el dueño de una sandbox es una persona del IdP, con roles | Aceptada |
| [0008](0008-network-flow-attribution.md) | Atribución de flujos de red a `owner_sub` | **Propuesta** (implementada en parte) |
| [0009](0009-agent-sessions.md) | Sesiones de agente como forma primaria de aislamiento | Aceptada |
| [0010](0010-on-demand-local-net.md) | Red local bajo demanda: un túnel completo iniciado por el agente local | Aceptada |
| [0011](0011-multi-node.md) | Varios nodos: identidad, canal con el plano de control, colocación por capacidad y nodos caídos | Aceptada |
| [0012](0012-retained-disks.md) | Parar no es borrar: el disco de una sandbox sobrevive a la parada | Aceptada |
| 0013 | *(reservado)* Reevaluar Kubernetes con un spike medido ([#143](https://github.com/luisgf/agent-sandbox-platform/issues/143)) | — |
| [0014](0014-vms-outlive-the-agent.md) | Las VMs sobreviven al node-agent y el siguiente proceso las adopta | Aceptada |
| [0015](0015-unprivileged-vmm.md) | El VMM corre como un usuario sin privilegios, uno por VM | Aceptada |
| [0016](0016-single-host.md) | Un solo host: SQLite y `asp-server`, un servidor que se configura solo | Aceptada |

## Estados

| Estado | Significa |
|---|---|
| **Propuesta** | Se escribió para discutirla. Nada del código depende de ella (o solo una parte: la línea `Implementación` dice cuál) |
| **Aceptada** | Es la decisión vigente. Que esté implementada del todo, en parte o no lo dice la línea `Implementación`, no el estado |
| **Obsoleta** | Ya no se sigue y nada la sustituye |
| **Sustituida por NNNN** | La reemplaza otro ADR, que la enlaza |

## Cómo se escribe uno

- Cabecera: `Estado` (uno de los de arriba), `Fecha`, y cuando haga falta `Implementación`, `Extiende`, `Enmendada por` y `Relacionados`.
- Secciones: **Contexto**, **Decisión**, **Alternativas consideradas**, **Consecuencias** (positivas, negativas) y **Límites honestos**.
- **No se reescribe la historia.** Cuando el código cambia lo que un ADR decía, se añade una nota fechada en **Enmiendas** (y, si hace falta, se tacha el texto que ya no vale), no se borra el razonamiento. La excepción son los cuerpos que describen el estado de hoy («qué hay en el código»), que se mantienen al día.
- El título nombra la función, no la fase del roadmap en que se hizo.
- Los ADR 0003, 0007 y 0011 son el modelo de identidad y de confianza; para leerlos juntos, [la arquitectura](../architecture.md).

`make check-docs` comprueba que cada ADR tiene un estado válido y una fila en esta tabla.
