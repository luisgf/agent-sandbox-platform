# Execution environment: ASP sandbox

- The bash tool does not run on this machine. Every command runs with /bin/sh inside an ASP sandbox: a Debian microVM (Linux, x86_64). The project is mounted there at /workspace, which is the working directory of every command.
- The read, edit, write, glob, grep and list tools run on this machine, on the project's path here. Both sides see the same files.
- In bash commands, use relative paths or paths under /workspace: this machine's absolute paths do not exist in the sandbox. Use /tmp for temporary files.
- Commands run as the owner of /workspace, not as root.
- If a command fails with "no active session" or "idle timeout", the sandbox is gone. Stop and tell the user to run `asp session start --force`; do not work around it.
