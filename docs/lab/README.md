# El laboratorio

Esta carpeta describe **el host de pruebas de los mantenedores**, no cómo desplegar ASP: es el registro de un servidor concreto (un host KVM con el plano de control, el node-agent, un Postgres y, de otra aplicación, un Keycloak), para quien tenga que tocarlo, y la constancia de dónde se ha comprobado cada cosa con VMs de verdad. Nada del producto depende de ella. Las páginas del producto no citan sus nombres, rutas ni puertos: usan `https://cp.example:8443`, `idp.example.org` y similares.

| Página | Qué es |
|---|---|
| [ncc1701d.md](ncc1701d.md) | El host: qué corre, las unidades de systemd y sus drop-ins (copias en [`ncc1701d/`](ncc1701d/)), cómo se despliega y se vuelve atrás, cómo se prueba sin tocar lo que corre |
| [idp-keycloak.md](idp-keycloak.md) | El realm `asp` de su Keycloak y cómo está cableado al plano de control |

Para desplegar ASP en un host propio: [instalar](../how-to/install.md), o [un solo host](../how-to/single-host.md). Para conectar tu IdP: [conectar un IdP](../how-to/idp.md).
