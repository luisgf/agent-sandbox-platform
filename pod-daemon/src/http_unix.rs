//! HTTP over a Unix domain socket.

use crate::http_serve;
use std::fs;
use std::io;
use std::os::unix::net::UnixListener;
use std::path::PathBuf;
use std::thread;
use std::time::Duration;

pub fn serve(path: PathBuf, exec_timeout: Duration) -> io::Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    if path.exists() {
        fs::remove_file(&path)?;
    }
    let listener = UnixListener::bind(&path)?;
    println!("listening on unix://{} (HTTP JSON)", path.display());

    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let timeout = exec_timeout;
                thread::spawn(move || {
                    if let Err(err) = http_serve::handle_connection(stream, timeout) {
                        eprintln!("connection error: {err}");
                    }
                });
            }
            Err(error) => eprintln!("accept error: {error}"),
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::os::unix::net::UnixStream;
    use std::sync::mpsc;
    use std::thread;

    #[test]
    fn healthz_and_exec_echo() {
        let dir = tempfile_dir();
        let sock = dir.join("pod.sock");
        if let Some(parent) = sock.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        let _ = fs::remove_file(&sock);
        let listener = UnixListener::bind(&sock).unwrap();
        let (ready_tx, ready_rx) = mpsc::channel();
        let timeout = Duration::from_secs(5);
        thread::spawn(move || {
            ready_tx.send(()).ok();
            for _ in 0..2 {
                let (stream, _) = listener.accept().expect("accept");
                http_serve::handle_connection(stream, timeout).expect("handle");
            }
        });
        ready_rx.recv().unwrap();
        thread::sleep(Duration::from_millis(20));

        let mut stream = UnixStream::connect(&sock).expect("connect");
        stream
            .write_all(b"GET /healthz HTTP/1.1\r\nHost: local\r\nConnection: close\r\n\r\n")
            .unwrap();
        let mut buf = Vec::new();
        stream.read_to_end(&mut buf).unwrap();
        let resp = String::from_utf8_lossy(&buf);
        assert!(resp.contains("200"), "{resp}");
        assert!(resp.contains("\"status\":\"ok\""), "{resp}");

        let mut stream = UnixStream::connect(&sock).expect("connect");
        let body = br#"{"cmd":["echo","hello"]}"#;
        let req = format!(
            "POST /v1/exec HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        );
        stream.write_all(req.as_bytes()).unwrap();
        stream.write_all(body).unwrap();
        let mut buf = Vec::new();
        stream.read_to_end(&mut buf).unwrap();
        let resp = String::from_utf8_lossy(&buf);
        assert!(resp.contains("200"), "{resp}");
        assert!(resp.contains("hello"), "{resp}");
        assert!(resp.contains("\"exit_code\":0"), "{resp}");

        let _ = fs::remove_file(&sock);
        let _ = fs::remove_dir(&dir);
    }

    fn tempfile_dir() -> PathBuf {
        let mut p = std::env::temp_dir();
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        p.push(format!("pod-daemon-test-{}-{}", std::process::id(), nanos));
        let _ = fs::create_dir_all(&p);
        p
    }
}
