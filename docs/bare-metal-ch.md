# Operación bare-metal con Cloud Hypervisor (esta guía se partió)

Esta guía juntaba la instalación, el diseño de la red, el funcionamiento interno de un nodo, la operación de seguridad y el diagnóstico en un solo fichero de casi mil líneas, y se contradecía a sí misma. Cada tema tiene ahora su página:

| Buscabas (apartado de la guía) | Ahora |
|---|---|
| 1–2: prerrequisitos, Cloud Hypervisor, kernel e imagen del guest, directorios | [Instalar un nodo](how-to/install-node.md) |
| 3: red de las sandboxes, TAP, NAT, DNS, proxy, nftables | [Red de las sandboxes y egress](concepts/networking-and-egress.md) |
| 4: Postgres, TLS, la CA, las credenciales del plano de control | [Instalar el plano de control](how-to/install-control-plane.md) |
| 5.1–5.4, 5.6, 5.7, 6 y el workspace: un proceso por VM, reinicios del agente, limpieza, VMM sin privilegios, imagen, vsock y `exec` | [Cómo ejecuta un nodo las sandboxes](concepts/node-runtime.md) |
| 5.5: plano de control en otro host (mTLS en los dos sentidos) | [Varios servidores](ops-multi-node.md) |
| 7 y 10: comprobar un nodo, el procedimiento de punta a punta | [Instalar un nodo, apartado «Comprobar»](how-to/install-node.md#6-comprobar) |
| 8–8e: endurecer, rotar claves y certificados, mTLS estricto, atestación, fencing, SSH | [Operaciones de seguridad](how-to/security-operations.md) |
| 9: troubleshooting | [Diagnóstico](how-to/troubleshooting.md) |
