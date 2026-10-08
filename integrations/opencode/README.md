# OpenCode integration

Runs OpenCode's bash tool inside an ASP session. Setup guide: [Usar ASP con OpenCode](../../docs/getting-started/opencode.md) (in Spanish, like the rest of `docs/`).

| File | Install as | What it does |
|---|---|---|
| `asp-opencode-shell` | `~/.local/bin/asp-opencode-shell`, set as `"shell"` | Runs each `-c` command with `/bin/sh -c` in the session (`asp session exec --no-pty`) and returns its exit code. Runs as the owner of `/workspace` (the guest's pod-daemon does it) unless `ASP_GUEST_AS_ROOT=1`, which passes `asp --root`. With `ASP_SSH`, runs `asp` on another host over ssh. |
| `plugins/asp-sandbox.ts` | `.opencode/plugins/asp-sandbox.ts` in the project | Refuses to run a bash command when the configured shell is missing (OpenCode would fall back to the host shell), turns `workdir` into `ASP_GUEST_CWD` for the wrapper, and rewrites the OS, temporary directory and, in the bash-only setup, working directory and platform that OpenCode tells the model. |
| `instructions/bash-only.md`, `instructions/same-host.md` | `.opencode/asp-sandbox.md` | Tells the model where its commands run and which paths exist there. |
| `opencode.bash-only.json`, `opencode.same-host.json` | `opencode.json` in the project | Shell, instructions, and permissions for each setup. |

The plugin is active when the OpenCode config sets `"shell"` or `ASP_SESSION_NAME` is set, and does nothing otherwise. It was tested with OpenCode 1.18.34. Its system prompt edit uses the experimental `experimental.chat.system.transform` hook, so check it after upgrading OpenCode.
