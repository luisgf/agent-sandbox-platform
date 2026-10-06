mod exec_session;
mod pty;
mod http_serve;
mod http_tcp;
mod http_unix;
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

    /// TCP bind address when --listen=tcp (lab/TAP).
    #[arg(long, default_value = "0.0.0.0:26500")]
    tcp_addr: String,

    /// Timeout of a buffered exec (POST /v1/exec without ?stream=1), in seconds.
    /// A streamed exec has no overall timeout.
    #[arg(long, default_value_t = 30)]
    exec_timeout_secs: u64,

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

    let limits = http_serve::ExecLimits {
        buffered: Duration::from_secs(args.exec_timeout_secs),
        stream_idle: (args.stream_idle_timeout_secs > 0).then(|| Duration::from_secs(args.stream_idle_timeout_secs)),
    };
    match args.listen {
        ListenMode::Unix => http_unix::serve(args.unix_socket, limits),
        ListenMode::Tcp => http_tcp::serve(&args.tcp_addr, limits),
        ListenMode::Vsock => {
            #[cfg(target_os = "linux")]
            {
                vsock_linux::serve(args.vsock_port, limits)
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
