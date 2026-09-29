//! Guest-side SSH agent unix listener that dials host vsock (or unix upstream).
//!
//! Enabled with `--ssh-auth-bridge`. Prefer the dedicated `vsock-ssh-agent-proxy`
//! systemd unit for production; this path keeps a single binary usable in lab.

use std::io::{self};
use std::os::unix::net::{UnixListener, UnixStream};
use std::path::Path;
use std::thread;

/// Listen on `listen_path` and byte-pump each connection to upstream.
/// Upstream is `ASP_SSH_AGENT_UPSTREAM` (unix path) when set, else AF_VSOCK cid:port.
pub fn spawn(listen_path: &Path, cid: u32, port: u32) -> io::Result<()> {
    if let Some(parent) = listen_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let _ = std::fs::remove_file(listen_path);
    let listener = UnixListener::bind(listen_path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(listen_path, std::fs::Permissions::from_mode(0o660));
    }
    let upstream_env = std::env::var("ASP_SSH_AGENT_UPSTREAM").ok();
    println!(
        "ssh-auth-bridge listening on {} → {}",
        listen_path.display(),
        describe(&upstream_env, cid, port)
    );
    thread::spawn(move || {
        for conn in listener.incoming() {
            match conn {
                Ok(client) => {
                    let up = upstream_env.clone();
                    thread::spawn(move || {
                        if let Err(err) = handle(client, up.as_deref(), cid, port) {
                            eprintln!("ssh-auth-bridge session: {err}");
                        }
                    });
                }
                Err(err) => eprintln!("ssh-auth-bridge accept: {err}"),
            }
        }
    });
    Ok(())
}

fn describe(upstream: &Option<String>, cid: u32, port: u32) -> String {
    if let Some(u) = upstream {
        return u.clone();
    }
    format!("vsock://{cid}:{port}")
}

fn handle(client: UnixStream, upstream: Option<&str>, cid: u32, port: u32) -> io::Result<()> {
    if let Some(path) = upstream {
        let path = path.strip_prefix("unix:").unwrap_or(path);
        let up = UnixStream::connect(path)?;
        return pump_unix(client, up);
    }
    #[cfg(target_os = "linux")]
    {
        let up = crate::vsock_linux::VsockStream::connect(cid, port)?;
        return pump_vsock(client, up);
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (cid, port);
        Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "vsock dial requires Linux AF_VSOCK (set ASP_SSH_AGENT_UPSTREAM=unix:/path for lab)",
        ))
    }
}

fn pump_unix(mut a: UnixStream, mut b: UnixStream) -> io::Result<()> {
    let mut a2 = a.try_clone()?;
    let mut b2 = b.try_clone()?;
    let t = thread::spawn(move || {
        let _ = io::copy(&mut b2, &mut a2);
    });
    let _ = io::copy(&mut a, &mut b);
    let _ = t.join();
    Ok(())
}

#[cfg(target_os = "linux")]
fn pump_vsock(mut a: UnixStream, mut b: crate::vsock_linux::VsockStream) -> io::Result<()> {
    let mut a2 = a.try_clone()?;
    let mut b2 = b.try_clone()?;
    let t = thread::spawn(move || {
        let _ = io::copy(&mut b2, &mut a2);
    });
    let _ = io::copy(&mut a, &mut b);
    let _ = t.join();
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::path::PathBuf;
    use std::time::Duration;

    #[test]
    fn unix_upstream_identities_contract() {
        let dir = tempfile_dir();
        let upstream = dir.join("up.sock");
        let listen = dir.join("listen.sock");

        let up_ln = UnixListener::bind(&upstream).unwrap();
        thread::spawn(move || {
            if let Ok((mut c, _)) = up_ln.accept() {
                let mut hdr = [0u8; 4];
                let _ = c.read_exact(&mut hdr);
                let n = u32::from_be_bytes(hdr) as usize;
                let mut body = vec![0u8; n];
                let _ = c.read_exact(&mut body);
                let resp = [12u8, 0, 0, 0, 0];
                let mut out = Vec::new();
                out.extend_from_slice(&(resp.len() as u32).to_be_bytes());
                out.extend_from_slice(&resp);
                let _ = c.write_all(&out);
            }
        });

        std::env::set_var("ASP_SSH_AGENT_UPSTREAM", format!("unix:{}", upstream.display()));
        spawn(&listen, 2, 26501).unwrap();
        thread::sleep(Duration::from_millis(80));

        let mut client = UnixStream::connect(&listen).unwrap();
        let req = [0u8, 0, 0, 1, 11];
        client.write_all(&req).unwrap();
        let mut hdr = [0u8; 4];
        client.read_exact(&mut hdr).unwrap();
        let n = u32::from_be_bytes(hdr) as usize;
        let mut body = vec![0u8; n];
        client.read_exact(&mut body).unwrap();
        assert_eq!(body[0], 12);
        std::env::remove_var("ASP_SSH_AGENT_UPSTREAM");
    }

    fn tempfile_dir() -> PathBuf {
        let mut d = std::env::temp_dir();
        d.push(format!("asp-ssh-proxy-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&d);
        d
    }
}
