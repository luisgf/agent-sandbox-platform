// asp-sandbox: an OpenCode plugin for running the bash tool inside an ASP
// session through asp-opencode-shell. It is active when the OpenCode config
// sets "shell" or ASP_SESSION_NAME is set, and then:
//
//   - refuses to run a bash command when the configured shell is missing or
//     not executable. OpenCode would otherwise fall back to the host's
//     /bin/zsh or bash without a word;
//   - takes the `workdir` argument out of the call and hands the matching
//     guest directory to the wrapper (ASP_GUEST_CWD). OpenCode would otherwise
//     spawn the wrapper in that directory on the host, where it may not exist;
//   - tells the model the bash tool runs on Linux in the sandbox: OS and
//     temporary directory in the tool description;
//   - with the file tools denied ("edit": "deny", the bash-only setup), also
//     replaces the host working directory and platform in the system prompt
//     and drops the advice to use the file tools instead of bash.
//
// Tested with OpenCode 1.18.34. The system prompt edit uses the experimental
// hook experimental.chat.system.transform.
import type { Plugin } from "@opencode-ai/plugin"
import { accessSync, constants } from "node:fs"
import path from "node:path"

const GUEST_ROOT = process.env.ASP_WORKSPACE_GUEST ?? "/workspace"

export const AspSandbox: Plugin = async ({ client, directory, worktree }) => {
  const hostRoot = process.env.ASP_WORKSPACE_HOST ?? worktree ?? directory
  const guestCwd = new Map<string, string>()

  let config: { shell?: string; bashOnly: boolean } | undefined
  const settings = async () => {
    if (!config) {
      const data: any = (await client.config.get()).data ?? {}
      config = { shell: data.shell, bashOnly: data.permission?.edit === "deny" }
    }
    return config
  }
  const active = async () => Boolean((await settings()).shell || process.env.ASP_SESSION_NAME)

  const requireShell = async () => {
    const { shell } = await settings()
    if (!shell) throw new Error('asp-sandbox: ASP_SESSION_NAME is set but the OpenCode config has no "shell"; refusing to run the command on the host')
    try {
      accessSync(shell, constants.X_OK)
    } catch {
      throw new Error(`asp-sandbox: shell ${shell} is missing or not executable; refusing to run the command on the host`)
    }
  }

  const toGuest = (dir: string) => {
    if (dir === GUEST_ROOT || dir.startsWith(GUEST_ROOT + "/")) return path.posix.normalize(dir)
    if (!path.isAbsolute(dir)) return path.posix.join(GUEST_ROOT, dir)
    const rel = path.relative(hostRoot, dir)
    if (rel === "" || (!rel.startsWith("..") && !path.isAbsolute(rel))) return path.posix.join(GUEST_ROOT, rel)
    return path.posix.normalize(dir) // any other absolute path names a guest directory
  }

  return {
    "tool.definition": async ({ toolID }, output) => {
      if (toolID !== "bash" || !(await active())) return
      output.description = output.description
        .replace(/OS: [^,]+,/, "OS: linux (ASP sandbox, a Debian microVM),")
        .replace(/Use `[^`]+` for temporary work[^\n]*/, "Use `/tmp` inside the sandbox for temporary work.")
      if ((await settings()).bashOnly)
        output.description = output.description.replace(
          /IMPORTANT: This tool is for terminal operations[^\n]*/,
          `This tool runs inside the ASP sandbox, in ${GUEST_ROOT}. The file tools are disabled, so use it for file operations too.`,
        )
    },
    "experimental.chat.system.transform": async (_input, output) => {
      if (!(await active()) || !(await settings()).bashOnly) return
      // Edit in place: OpenCode keeps its own reference to the array.
      output.system.forEach((text, i) => {
        output.system[i] = text
          .replaceAll(`Working directory: ${directory}`, `Working directory: ${GUEST_ROOT} (inside the ASP sandbox)`)
          .replaceAll(`Workspace root folder: ${worktree}`, `Workspace root folder: ${GUEST_ROOT}`)
          .replaceAll(`Platform: ${process.platform}`, "Platform: linux")
      })
    },
    "tool.execute.before": async ({ tool, callID }, output) => {
      if (tool !== "bash" || !(await active())) return
      await requireShell()
      if (output.args?.workdir) {
        guestCwd.set(callID, toGuest(output.args.workdir))
        delete output.args.workdir
      }
    },
    "shell.env": async ({ callID }, output) => {
      const cwd = callID ? guestCwd.get(callID) : undefined
      if (!cwd) return
      output.env.ASP_GUEST_CWD = cwd
      guestCwd.delete(callID!)
    },
  }
}
