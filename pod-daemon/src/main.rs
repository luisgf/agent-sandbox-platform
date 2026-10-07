mod exec_session;
mod http_serve;
mod http_tcp;
mod http_unix;
mod peer;
mod pty;
mod runas;
mod ssh_agent_proxy;
#[cfg(target_os = "linux")]
mod vsock_linux;

use clap::{Parser, ValueEnum};
use std::io;
use std::path::PathBuf;
use std::time::Duration;

#[derive(Clone, Debug, ValueEnum)]
enum ListenMode {
    Unix,
    /// AF_VSOCK in the guest (productive Cloud Hypervisor path).
    Vsock,
    /// TCP listen inside the guest (TAP/lab alternate; host dials guest IP, not vsock).
    Tcp,
}

#[derive(Debug, Parser)]
#[command(version, about = "Guest-side sandbox daemon")]
struct Args {
    /// Transport to listen on.
    #[arg(long, value_enum, default_value_t = ListenMode::Unix)]
    listen: ListenMode,

    /// Development Unix socket used when --listen=unix.
    #[arg(long, default_value = "/run/agent-sandbox/pod-daemon.sock")]
    unix_socket: PathBuf,

    /// Guest vsock port (AF_VSOCK) when --listen=vsock. Host dials via CH hybrid UDS CONNECT.
    #[arg(long, default_value_t = 26500)]
    vsock_port: u32,

    /// TCP bind when --listen=tcp (lab/TAP): `auto` or `auto:PORT` (the address of
    /// the interface that holds the default route, which is the TAP), or IP:PORT.
    /// A wildcard address is refused: anything that reaches the daemon can run
    /// commands.
    #[arg(long, default_value = "auto")]
    tcp_addr: String,

    /// With --listen=tcp, the one peer address answered. Default: the default
    /// gateway, which is the host's end of the TAP.
    #[arg(long)]
    tcp_peer: Option<String>,

    /// Allow --tcp-addr to be a wildcard address (0.0.0.0, ::). Lab only.
    #[arg(long, default_value_t = false)]
    tcp_any_interface: bool,

    /// Account commands run as when the workspace has no non-root owner. A command
    /// runs as root only when its request says as_root. `none` runs commands as
    /// the daemon itself (the old behaviour). Needs the daemon to run as root.
    #[arg(long, default_value = "sandboxd")]
    exec_user: String,

    /// Guest mount point of the host workspace: commands run as the owner of this
    /// directory when that owner is not root, so files they write keep the host
    /// user's uid.
    #[arg(long, default_value = "/workspace")]
    workspace_dir: PathBuf,

    /// Most processes: RLIMIT_NPROC of each command and pids.max of the exec
    /// cgroup. 0 sets neither.
    #[arg(long, default_value_t = 4096)]
    exec_max_procs: u64,

    /// RLIMIT_NOFILE of each command. 0 leaves it alone.
    #[arg(long, default_value_t = 65536)]
    exec_max_open_files: u64,

    /// memory.max of the exec cgroup, in percent of the guest's memory. 0 sets none.
    #[arg(long, default_value_t = 80)]
    exec_max_memory_percent: u64,

    /// Let commands write core dumps (off: RLIMIT_CORE is 0).
    #[arg(long, default_value_t = false)]
    exec_core_dumps: bool,

    /// cgroup2 directory every command joins, created with the limits above.
    /// Empty disables it. Without cgroup2, or as non-root, it is skipped.
    #[arg(long, default_value = "/sys/fs/cgroup/asp-exec")]
    exec_cgroup: String,

    /// Timeout of a buffered exec (POST /v1/exec without ?stream=1), in seconds.
    /// A streamed exec has no overall timeout.
    #[arg(long, default_value_t = 30)]
    exec_timeout_secs: u64,

    /// The longest a request may ask a buffered exec to run (`timeout_secs`).
    #[arg(long, default_value_t = 3600)]
    exec_max_timeout_secs: u64,

    /// Kill a streamed exec after this many seconds without output and without
    /// stdin. 0 (default): a stream lasts as long as its command and its client.
    #[arg(long, default_value_t = 0)]
    stream_idle_timeout_secs: u64,

    /// Expose host SSH agent at --ssh-auth-socket via vsock CID ASP_HOST_CID:26501
    /// (or ASP_SSH_AGENT_UPSTREAM=unix:/path for lab). Prefer systemd unit
    /// ssh-agent-vsock.service + vsock-ssh-agent-proxy binary in the guest image.
    #[arg(long, default_value_t = false)]
    ssh_auth_bridge: bool,

    /// Guest path for the proxied SSH_AUTH_SOCK.
    #[arg(long, default_value = "/run/agent-sandbox/ssh-agent.sock")]
    ssh_auth_socket: PathBuf,

    /// Guest Unix socket for POST /v1/tokens/oidc requests (aud only).
    /// Prefer dialing host vsock CID (ASP_HOST_CID, default 2) port 26502 in productive mode.
    #[arg(long, default_value = "/run/agent-sandbox/identity.sock")]
    identity_socket: PathBuf,

    /// Expected path of a host-forwarded SSH agent socket (virtiofs or socat→vsock:26501).
    /// Alias of --ssh-auth-socket for operators; does not create the socket itself.
    #[arg(long)]
    ssh_auth_sock: Option<PathBuf>,
}

fn main() -> io::Result<()> {
    let args = Args::parse();
    let host_cid = std::env::var("ASP_HOST_CID").unwrap_or_else(|_| "2".into());
    if let Some(ref sock) = args.ssh_auth_sock {
        // Operators may pass --ssh-auth-sock as the expected forwarded path.
        let _ = sock;
    }
    println!(
        "pod-daemon v{} starting: transport={:?}, vsock_port={}, tcp_addr={}, exec_timeout_secs={}, stream_idle_timeout_secs={}, ssh_auth_bridge={}, identity_socket={}, asp_host_cid={}",
        env!("CARGO_PKG_VERSION"),
        args.listen,
        args.vsock_port,
        args.tcp_addr,
        args.exec_timeout_secs,
        args.stream_idle_timeout_secs,
        args.ssh_auth_bridge,
        args.identity_socket.display(),
        host_cid
    );

    if args.ssh_auth_bridge {
        let sock = args
            .ssh_auth_sock
            .clone()
            .unwrap_or_else(|| args.ssh_auth_socket.clone());
        let cid: u32 = host_cid.parse().unwrap_or(2);
        if let Err(err) = ssh_agent_proxy::spawn(&sock, cid, 26501) {
            eprintln!("ssh-auth-bridge failed to start: {err}");
        }
    }

    let policy: &'static runas::ExecPolicy = Box::leak(Box::new(build_policy(&args)));
    let limits = http_serve::ExecLimits {
        buffered: Duration::from_secs(args.exec_timeout_secs),
        max_buffered: Duration::from_secs(args.exec_max_timeout_secs.max(args.exec_timeout_secs)),
        stream_idle: (args.stream_idle_timeout_secs > 0).then(|| Duration::from_secs(args.stream_idle_timeout_secs)),
        policy,
    };
    match args.listen {
        ListenMode::Unix => http_unix::serve(args.unix_socket, limits),
        ListenMode::Tcp => {
            let route = std::fs::read_to_string("/proc/net/route")
                .ok()
                .and_then(|t| peer::parse_default_route(&t));
            let bind = peer::resolve_tcp(
                &args.tcp_addr,
                args.tcp_peer.as_deref(),
                args.tcp_any_interface,
                route,
                peer::iface_ipv4,
            )
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;
            http_tcp::serve(&bind, limits)
        }
        ListenMode::Vsock => {
            #[cfg(target_os = "linux")]
            {
                let host_cid_n: u32 = host_cid.parse().unwrap_or(peer::HOST_CID);
                vsock_linux::serve(args.vsock_port, limits, host_cid_n)
            }
            #[cfg(not(target_os = "linux"))]
            {
                Err(io::Error::new(
                    io::ErrorKind::Unsupported,
                    "vsock transport requires Linux AF_VSOCK",
                ))
            }
        }
    }
}

/// The account flag: empty or `none` turns user switching off.
fn exec_user_flag(value: &str) -> Option<String> {
    match value.trim() {
        "" | "none" => None,
        name => Some(name.to_string()),
    }
}

/// Builds how execs are run from the flags, creating the exec cgroup when it can.
fn build_policy(args: &Args) -> runas::ExecPolicy {
    let limits = runas::Limits {
        max_procs: args.exec_max_procs,
        max_open_files: args.exec_max_open_files,
        core_dumps: args.exec_core_dumps,
    };
    let default_user = exec_user_flag(&args.exec_user);
    let mut cgroup = None;
    let cgroup_path = PathBuf::from(args.exec_cgroup.trim());
    if !args.exec_cgroup.trim().is_empty() && runas::euid() == 0 {
        if let (Some(root), Some(name)) = (cgroup_path.parent(), cgroup_path.file_name()) {
            let mem_total = std::fs::read_to_string("/proc/meminfo")
                .ok()
                .and_then(|m| runas::parse_mem_total_kib(&m));
            match runas::setup_cgroup(
                root,
                &name.to_string_lossy(),
                args.exec_max_procs,
                args.exec_max_memory_percent,
                mem_total,
            ) {
                Ok(dir) => cgroup = Some(dir),
                Err(err) => eprintln!(
                    "exec cgroup {} not set up ({err}): commands run without a cgroup limit",
                    cgroup_path.display()
                ),
            }
        }
    }
    println!(
        "exec policy: user={:?} workspace_dir={} max_procs={} max_open_files={} core_dumps={} cgroup={} (running as uid {})",
        default_user,
        args.workspace_dir.display(),
        limits.max_procs,
        limits.max_open_files,
        limits.core_dumps,
        cgroup.as_ref().map_or("none".to_string(), |d| d.display().to_string()),
        runas::euid()
    );
    runas::ExecPolicy {
        default_user,
        workspace_dir: args.workspace_dir.clone(),
        limits,
        cgroup,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_exec_user_flag() {
        assert_eq!(exec_user_flag("sandboxd"), Some("sandboxd".into()));
        assert_eq!(exec_user_flag(" dev "), Some("dev".into()));
        assert_eq!(exec_user_flag("none"), None);
        assert_eq!(exec_user_flag(""), None);
    }
}
