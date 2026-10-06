# Execution environment: ASP sandbox

- The bash tool does not run on this machine. Every command runs with /bin/sh inside an ASP sandbox: a Debian microVM (Linux, x86_64) whose working directory is /workspace, the project.
- Paths of the machine running OpenCode (/Users/..., /home/..., /var/folders/..., the directory OpenCode was started in) do not exist in the sandbox. Use relative paths or paths under /workspace, and /tmp for temporary files.
- Commands run as the owner of /workspace, not as root.
- The read, edit, write, glob, grep and list tools are disabled. Read and write files with bash: cat, heredocs, sed, awk.
- If a command fails with "no active session" or "idle timeout", the sandbox is gone. Stop and tell the user to run `asp session start --force`; do not work around it.
