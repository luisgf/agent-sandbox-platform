//! Shared HTTP/1.1 JSON handler for unix / TCP / vsock transports.

use serde::{Deserialize, Serialize};
use std::fs::File;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::Mutex;
use std::thread;
use std::time::{Duration, Instant};

use crate::exec_session::{self, StdinSlot};
use crate::pty::{self, PtyCommand};

#[derive(Debug, Deserialize)]
struct ExecRequest {
    cmd: Vec<String>,
    #[serde(default)]
    env: Option<std::collections::HashMap<String, String>>,
    #[serde(default)]
    cwd: Option<String>,
    /// Run under a PTY. Stderr is merged into the master (no separate stderr events).
    #[serde(default)]
    pty: bool,
    #[serde(default)]
    rows: u16,
    #[serde(default)]
    cols: u16,
    /// One-shot stdin for the buffered JSON exec.
    #[serde(default)]
    stdin: Option<String>,
    /// Non-PTY stream: keep a pipe open and accept POST /v1/exec/stdin.
    #[serde(default)]
    stdin_stream: bool,
}

#[derive(Debug, Deserialize)]
struct StdinRequest {
    exec_id: String,
    #[serde(default)]
    data: String,
    #[serde(default)]
    close: bool,
    #[serde(default)]
    rows: u16,
    #[serde(default)]
    cols: u16,
}

#[derive(Debug, Serialize)]
struct ExecResponse {
    stdout: String,
    stderr: String,
    exit_code: i32,
}

#[derive(Debug, Serialize)]
struct ErrorBody {
    error: String,
}

/// Serve one HTTP request/response on any stream (Unix, TCP, or AF_VSOCK).
pub fn handle_connection<S: Read + Write>(mut stream: S, exec_timeout: Duration) -> io::Result<()> {
    let (method, path, body) = {
        let mut reader = BufReader::new(&mut stream);
        let mut request_line = String::new();
        reader.read_line(&mut request_line)?;
        if request_line.is_empty() {
            return Ok(());
        }
        let parts: Vec<&str> = request_line.trim_end().split_whitespace().collect();
        if parts.len() < 2 {
            return write_response(&mut stream, 400, r#"{"error":"bad request line"}"#);
        }
        let method = parts[0].to_string();
        let path = parts[1].to_string();

        let mut content_length = 0usize;
        loop {
            let mut line = String::new();
            reader.read_line(&mut line)?;
            if line == "\r\n" || line == "\n" || line.is_empty() {
                break;
            }
            let lower = line.to_ascii_lowercase();
            if let Some(rest) = lower.strip_prefix("content-length:") {
                content_length = rest.trim().parse().unwrap_or(0);
            }
        }

        let mut body = vec![0u8; content_length];
        if content_length > 0 {
            reader.read_exact(&mut body)?;
        }
        (method, path, body)
    };

    let (path_only, stream_exec) = path_and_stream(&path);
    match (method.as_str(), path_only) {
        ("GET", "/healthz") => write_response(&mut stream, 200, r#"{"status":"ok"}"#),
        ("POST", "/v1/exec/stdin") => handle_stdin(&mut stream, &body),
        ("POST", "/v1/exec") if stream_exec => {
            let req: ExecRequest = match serde_json::from_slice(&body) {
                Ok(r) => r,
                Err(e) => {
                    return write_json(
                        &mut stream,
                        400,
                        &ErrorBody {
                            error: format!("invalid JSON: {e}"),
                        },
                    );
                }
            };
            if req.cmd.is_empty() {
                return write_json(
                    &mut stream,
                    400,
                    &ErrorBody {
                        error: "cmd required".into(),
                    },
                );
            }
            run_exec_stream(&mut stream, &req, exec_timeout)
        }
        ("POST", "/v1/exec") => {
            let req: ExecRequest = match serde_json::from_slice(&body) {
                Ok(r) => r,
                Err(e) => {
                    return write_json(
                        &mut stream,
                        400,
                        &ErrorBody {
                            error: format!("invalid JSON: {e}"),
                        },
                    );
                }
            };
            if req.cmd.is_empty() {
                return write_json(
                    &mut stream,
                    400,
                    &ErrorBody {
                        error: "cmd required".into(),
                    },
                );
            }
            match run_exec(&req, exec_timeout) {
                Ok(resp) => write_json(&mut stream, 200, &resp),
                Err(e) => write_json(
                    &mut stream,
                    500,
                    &ErrorBody {
                        error: e.to_string(),
                    },
                ),
            }
        }
        _ => write_response(&mut stream, 404, r#"{"error":"not found"}"#),
    }
}

fn path_and_stream(raw: &str) -> (&str, bool) {
    let (path, query) = match raw.split_once('?') {
        Some((p, q)) => (p, q),
        None => (raw, ""),
    };
    let stream = query.split('&').any(|kv| {
        let kv = kv.trim();
        kv == "stream=1" || kv == "stream=true" || kv == "stream=yes"
    });
    (path, stream)
}

fn apply_cwd_env(command: &mut Command, req: &ExecRequest) {
    if let Some(cwd) = &req.cwd {
        if !cwd.is_empty() {
            command.current_dir(cwd);
        }
    }
    if let Some(env) = &req.env {
        for (k, v) in env {
            command.env(k, v);
        }
    }
}

fn spawn_command(req: &ExecRequest) -> io::Result<std::process::Child> {
    let mut command = Command::new(&req.cmd[0]);
    command
        .args(&req.cmd[1..])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    apply_cwd_env(&mut command, req);
    command.spawn()
}

fn spawn_with_stdin_pipe(req: &ExecRequest) -> io::Result<std::process::Child> {
    let mut command = Command::new(&req.cmd[0]);
    command
        .args(&req.cmd[1..])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    apply_cwd_env(&mut command, req);
    command.spawn()
}

struct SessionGuard(Option<String>);
impl Drop for SessionGuard {
    fn drop(&mut self) {
        if let Some(id) = self.0.take() {
            exec_session::remove(&id);
        }
    }
}

struct ChildGuard(Option<std::process::Child>);
impl Drop for ChildGuard {
    fn drop(&mut self) {
        if let Some(mut child) = self.0.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

/// NDJSON over chunked HTTP. Each line is `{"type":"stdout"|"stderr","data":"..."}`
/// and a final `{"type":"exit","exit_code":N}`. When `pty` or `stdin_stream` is
/// set, the first line is `{"type":"ready","exec_id":"..."}` and stdin arrives
/// on POST /v1/exec/stdin. The buffered JSON path is unchanged.
fn run_exec_stream<S: Write>(out: &mut S, req: &ExecRequest, timeout: Duration) -> io::Result<()> {
    let mut session = SessionGuard(None);
    let mut pty_reader: Option<File> = None;
    let child = if req.pty {
        let (child, master) = match pty::spawn(PtyCommand {
            cmd: req.cmd.clone(),
            cwd: req.cwd.clone(),
            env: req.env.clone(),
            rows: req.rows,
            cols: req.cols,
        }) {
            Ok(v) => v,
            Err(e) => {
                return write_json(out, 500, &ErrorBody { error: e.to_string() });
            }
        };
        let reader = master.try_clone()?;
        session.0 = Some(exec_session::register(StdinSlot::Pty(Mutex::new(master))));
        pty_reader = Some(reader);
        child
    } else if req.stdin_stream {
        let mut child = match spawn_with_stdin_pipe(req) {
            Ok(c) => c,
            Err(e) => {
                return write_json(out, 500, &ErrorBody { error: e.to_string() });
            }
        };
        let stdin = child.stdin.take();
        session.0 = Some(exec_session::register(StdinSlot::Pipe(Mutex::new(stdin))));
        child
    } else {
        match spawn_command(req) {
            Ok(c) => c,
            Err(e) => {
                return write_json(out, 500, &ErrorBody { error: e.to_string() });
            }
        }
    };
    let mut guard = ChildGuard(Some(child));
    let child = guard.0.as_mut().unwrap();
    let stdout = if pty_reader.is_some() {
        None
    } else {
        child.stdout.take()
    };
    let stderr = if pty_reader.is_some() {
        None
    } else {
        child.stderr.take()
    };
    let (tx, rx) = mpsc::channel::<(&'static str, Vec<u8>)>();
    let tx_out = tx.clone();
    let tx_err = tx.clone();
    drop(tx);
    if let Some(mut pipe) = pty_reader {
        thread::spawn(move || {
            pump_pipe(&mut pipe, "stdout", &tx_out);
        });
        drop(tx_err);
    } else {
        thread::spawn(move || {
            if let Some(mut pipe) = stdout {
                pump_pipe(&mut pipe, "stdout", &tx_out);
            }
        });
        thread::spawn(move || {
            if let Some(mut pipe) = stderr {
                pump_pipe(&mut pipe, "stderr", &tx_err);
            }
        });
    }

    write_chunked_headers(out)?;
    if let Some(id) = session.0.as_ref() {
        write_ready(out, id)?;
    }
    let started = Instant::now();
    let mut killed = false;
    loop {
        match rx.recv_timeout(Duration::from_millis(30)) {
            Ok((kind, data)) => {
                write_stream_event(out, kind, &data)?;
            }
            Err(RecvTimeoutError::Timeout) => {
                if !killed && started.elapsed() > timeout {
                    if let Some(child) = guard.0.as_mut() {
                        let _ = child.kill();
                    }
                    killed = true;
                }
                if killed && started.elapsed() > timeout + Duration::from_secs(2) {
                    break;
                }
            }
            Err(RecvTimeoutError::Disconnected) => break,
        }
    }
    let mut child = guard.0.take().unwrap();
    let status = child.wait()?;
    let code = if killed {
        124
    } else {
        status.code().unwrap_or(128)
    };
    if killed {
        let msg = format!("exec timed out after {}s", timeout.as_secs());
        write_stream_event(out, "stderr", msg.as_bytes())?;
    }
    let exit_line = format!("{{\"type\":\"exit\",\"exit_code\":{code}}}\n");
    write_chunk(out, exit_line.as_bytes())?;
    write_chunk_end(out)?;
    Ok(())
}

fn write_ready<S: Write>(out: &mut S, id: &str) -> io::Result<()> {
    let line = format!("{{\"type\":\"ready\",\"exec_id\":\"{id}\"}}\n");
    write_chunk(out, line.as_bytes())
}

fn handle_stdin<S: Write>(stream: &mut S, body: &[u8]) -> io::Result<()> {
    let req: StdinRequest = match serde_json::from_slice(body) {
        Ok(r) => r,
        Err(e) => {
            return write_json(
                stream,
                400,
                &ErrorBody {
                    error: format!("invalid JSON: {e}"),
                },
            );
        }
    };
    if req.exec_id.is_empty() {
        return write_json(
            stream,
            400,
            &ErrorBody {
                error: "exec_id required".into(),
            },
        );
    }
    match exec_session::write(&req.exec_id, req.data.as_bytes(), req.close, req.rows, req.cols) {
        Ok(()) => write_json(stream, 200, &serde_json::json!({"ok": true})),
        Err(e) if e.kind() == io::ErrorKind::NotFound => write_json(
            stream,
            404,
            &ErrorBody {
                error: e.to_string(),
            },
        ),
        Err(e) => write_json(
            stream,
            500,
            &ErrorBody {
                error: e.to_string(),
            },
        ),
    }
}

fn pump_pipe<R: Read>(pipe: &mut R, kind: &'static str, tx: &mpsc::Sender<(&'static str, Vec<u8>)>) {
    let mut buf = [0u8; 1024];
    loop {
        match pipe.read(&mut buf) {
            Ok(0) | Err(_) => break,
            Ok(n) => {
                if tx.send((kind, buf[..n].to_vec())).is_err() {
                    break;
                }
            }
        }
    }
}

fn write_stream_event<S: Write>(out: &mut S, kind: &str, data: &[u8]) -> io::Result<()> {
    let payload = serde_json::json!({
        "type": kind,
        "data": String::from_utf8_lossy(data),
    });
    let mut line = serde_json::to_vec(&payload).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    line.push(b'\n');
    write_chunk(out, &line)
}

fn write_chunked_headers<S: Write>(stream: &mut S) -> io::Result<()> {
    let header = "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n";
    stream.write_all(header.as_bytes())?;
    stream.flush()
}

fn write_chunk<S: Write>(stream: &mut S, body: &[u8]) -> io::Result<()> {
    let header = format!("{:x}\r\n", body.len());
    stream.write_all(header.as_bytes())?;
    stream.write_all(body)?;
    stream.write_all(b"\r\n")?;
    stream.flush()
}

fn write_chunk_end<S: Write>(stream: &mut S) -> io::Result<()> {
    stream.write_all(b"0\r\n\r\n")?;
    stream.flush()
}

fn run_exec(req: &ExecRequest, timeout: Duration) -> io::Result<ExecResponse> {
    if req.pty {
        return run_exec_pty_buffered(req, timeout);
    }
    let child = if req.stdin.is_some() {
        spawn_with_stdin_pipe(req)?
    } else {
        spawn_command(req)?
    };
    collect_output(child, req.stdin.as_deref(), timeout)
}

/// How long the buffered exec keeps reading after the command exits. A
/// background job that inherited stdout or stderr keeps the pipe open: its
/// later output is not waited for.
const AFTER_EXIT_GRACE: Duration = Duration::from_secs(1);
/// After a timeout kill, how long to keep collecting output already in flight.
const AFTER_KILL_GRACE: Duration = Duration::from_millis(200);
const POLL_EVERY: Duration = Duration::from_millis(20);

#[derive(Default)]
struct Collected {
    stdout: Vec<u8>,
    stderr: Vec<u8>,
}

impl Collected {
    fn push(&mut self, kind: &str, data: &[u8]) {
        if kind == "stderr" {
            self.stderr.extend_from_slice(data);
        } else {
            self.stdout.extend_from_slice(data);
        }
    }
}

/// Runs a spawned child to completion for the buffered JSON exec. stdout and
/// stderr are read while the child runs and stdin is written from its own
/// thread, so a command that writes more than a pipe holds (64 KiB on Linux),
/// or that echoes its input as it reads it, never blocks on a full pipe. On
/// timeout the child is killed and what it wrote so far comes back with exit
/// code 124.
fn collect_output(mut child: Child, stdin: Option<&str>, timeout: Duration) -> io::Result<ExecResponse> {
    let (tx, rx) = mpsc::channel::<(&'static str, Vec<u8>)>();
    if let Some(mut pipe) = child.stdout.take() {
        let tx = tx.clone();
        thread::spawn(move || pump_pipe(&mut pipe, "stdout", &tx));
    }
    if let Some(mut pipe) = child.stderr.take() {
        let tx = tx.clone();
        thread::spawn(move || pump_pipe(&mut pipe, "stderr", &tx));
    }
    drop(tx);
    if let Some(mut pipe) = child.stdin.take() {
        let data = stdin.unwrap_or_default().as_bytes().to_vec();
        // A child that exits without reading its input makes this write fail
        // with EPIPE: that is the command's business, not an exec error.
        // Dropping the pipe afterwards is the EOF.
        thread::spawn(move || {
            let _ = pipe.write_all(&data);
        });
    }

    let deadline = Instant::now() + timeout;
    let mut out = Collected::default();
    let mut pipes_open = true;
    let mut exited: Option<(ExitStatus, Instant)> = None;
    loop {
        if pipes_open {
            match rx.recv_timeout(POLL_EVERY) {
                Ok((kind, data)) => out.push(kind, &data),
                Err(RecvTimeoutError::Timeout) => {}
                // Both pipes reached EOF and everything they carried was received.
                Err(RecvTimeoutError::Disconnected) => pipes_open = false,
            }
        } else {
            thread::sleep(POLL_EVERY);
        }
        if exited.is_none() {
            if let Some(status) = child.try_wait()? {
                exited = Some((status, Instant::now()));
            }
        }
        let now = Instant::now();
        match exited {
            Some((status, at)) => {
                if !pipes_open || now >= at + AFTER_EXIT_GRACE || now >= deadline {
                    return Ok(ExecResponse {
                        stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
                        stderr: String::from_utf8_lossy(&out.stderr).into_owned(),
                        exit_code: status.code().unwrap_or(128),
                    });
                }
            }
            None if now >= deadline => break,
            None => {}
        }
    }
    let _ = child.kill();
    let _ = child.wait();
    drain_for(&rx, &mut out, AFTER_KILL_GRACE);
    let mut stderr = String::from_utf8_lossy(&out.stderr).into_owned();
    if !stderr.is_empty() && !stderr.ends_with('\n') {
        stderr.push('\n');
    }
    stderr.push_str(&format!("exec timed out after {}s", timeout.as_secs()));
    Ok(ExecResponse {
        stdout: String::from_utf8_lossy(&out.stdout).into_owned(),
        stderr,
        exit_code: 124,
    })
}

/// Collects what the pipe readers still hold, for at most `grace`. A process
/// that outlived the killed command can keep a pipe open; it is not waited for.
fn drain_for(rx: &mpsc::Receiver<(&'static str, Vec<u8>)>, out: &mut Collected, grace: Duration) {
    let until = Instant::now() + grace;
    loop {
        let left = until.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return;
        }
        match rx.recv_timeout(left) {
            Ok((kind, data)) => out.push(kind, &data),
            Err(_) => return,
        }
    }
}

fn run_exec_pty_buffered(req: &ExecRequest, timeout: Duration) -> io::Result<ExecResponse> {
    let (mut child, master) = pty::spawn(PtyCommand {
        cmd: req.cmd.clone(),
        cwd: req.cwd.clone(),
        env: req.env.clone(),
        rows: req.rows,
        cols: req.cols,
    })?;
    // Read the master before writing to it: with echo on, input comes back as
    // output, and a master nobody reads stops accepting writes.
    let mut reader = master.try_clone()?;
    let (tx, rx) = mpsc::channel::<Vec<u8>>();
    thread::spawn(move || {
        let mut buf = [0u8; 1024];
        loop {
            match reader.read(&mut buf) {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    if tx.send(buf[..n].to_vec()).is_err() {
                        break;
                    }
                }
            }
        }
    });
    if let Some(data) = req.stdin.clone() {
        let mut writer = master;
        thread::spawn(move || {
            if !data.is_empty() {
                let _ = writer.write_all(data.as_bytes());
            }
            // Ctrl-D so a canonical reader sees EOF after the buffered bytes.
            let _ = writer.write_all(&[0x04, 0x04]);
            let _ = writer.flush();
        });
    }
    let started = Instant::now();
    let mut stdout = Vec::new();
    loop {
        match child.try_wait()? {
            Some(status) => {
                while let Ok(chunk) = rx.try_recv() {
                    stdout.extend_from_slice(&chunk);
                }
                // give the reader a moment to drain
                thread::sleep(Duration::from_millis(30));
                while let Ok(chunk) = rx.try_recv() {
                    stdout.extend_from_slice(&chunk);
                }
                return Ok(ExecResponse {
                    stdout: String::from_utf8_lossy(&stdout).into_owned(),
                    stderr: String::new(),
                    exit_code: status.code().unwrap_or(128),
                });
            }
            None => {
                while let Ok(chunk) = rx.try_recv() {
                    stdout.extend_from_slice(&chunk);
                }
                if started.elapsed() > timeout {
                    let _ = child.kill();
                    let _ = child.wait();
                    break;
                }
                thread::sleep(Duration::from_millis(20));
            }
        }
    }
    Ok(ExecResponse {
        stdout: String::from_utf8_lossy(&stdout).into_owned(),
        stderr: format!("exec timed out after {}s", timeout.as_secs()),
        exit_code: 124,
    })
}

fn write_json<S: Write, T: Serialize>(stream: &mut S, status: u16, body: &T) -> io::Result<()> {
    let payload =
        serde_json::to_vec(body).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
    write_bytes(stream, status, &payload)
}

fn write_response<S: Write>(stream: &mut S, status: u16, body: &str) -> io::Result<()> {
    write_bytes(stream, status, body.as_bytes())
}

fn write_bytes<S: Write>(stream: &mut S, status: u16, body: &[u8]) -> io::Result<()> {
    let reason = match status {
        200 => "OK",
        400 => "Bad Request",
        404 => "Not Found",
        500 => "Internal Server Error",
        _ => "Error",
    };
    let header = format!(
        "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    stream.write_all(header.as_bytes())?;
    stream.write_all(body)?;
    stream.flush()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;
    use std::net::{TcpListener, TcpStream};
    use std::sync::mpsc;
    use std::thread;

    #[test]
    fn healthz_over_tcp() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let (ready_tx, ready_rx) = mpsc::channel();
        thread::spawn(move || {
            ready_tx.send(()).ok();
            let (stream, _) = listener.accept().unwrap();
            handle_connection(stream, Duration::from_secs(5)).unwrap();
        });
        ready_rx.recv().unwrap();
        let mut stream = TcpStream::connect(addr).unwrap();
        stream
            .write_all(b"GET /healthz HTTP/1.1\r\nHost: local\r\nConnection: close\r\n\r\n")
            .unwrap();
        let mut buf = Vec::new();
        stream.read_to_end(&mut buf).unwrap();
        let resp = String::from_utf8_lossy(&buf);
        assert!(resp.contains("200"), "{resp}");
        assert!(resp.contains("\"status\":\"ok\""), "{resp}");
    }

    #[test]
    fn exec_stream_ndjson_chunked() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let (ready_tx, ready_rx) = mpsc::channel();
        thread::spawn(move || {
            ready_tx.send(()).ok();
            let (stream, _) = listener.accept().unwrap();
            handle_connection(stream, Duration::from_secs(5)).unwrap();
        });
        ready_rx.recv().unwrap();
        let mut stream = TcpStream::connect(addr).unwrap();
        let body = br#"{"cmd":["echo","hello-stream"]}"#;
        let req = format!(
            "POST /v1/exec?stream=1 HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        );
        stream.write_all(req.as_bytes()).unwrap();
        stream.write_all(body).unwrap();
        let mut buf = Vec::new();
        stream.read_to_end(&mut buf).unwrap();
        let resp = String::from_utf8_lossy(&buf);
        let lower = resp.to_ascii_lowercase();
        assert!(lower.contains("transfer-encoding: chunked"), "{resp}");
        assert!(lower.contains("application/x-ndjson"), "{resp}");
        assert!(resp.contains("hello-stream"), "{resp}");
        assert!(resp.contains("\"type\":\"exit\""), "{resp}");
        assert!(resp.contains("\"exit_code\":0"), "{resp}");
    }

    fn serve_threaded() -> std::net::SocketAddr {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        thread::spawn(move || {
            for stream in listener.incoming() {
                if let Ok(stream) = stream {
                    thread::spawn(move || {
                        let _ = handle_connection(stream, Duration::from_secs(5));
                    });
                }
            }
        });
        addr
    }

    fn read_headers(r: &mut BufReader<TcpStream>) {
        loop {
            let mut line = String::new();
            r.read_line(&mut line).unwrap();
            if line == "\r\n" || line == "\n" || line.is_empty() {
                break;
            }
        }
    }

    fn read_chunk(r: &mut BufReader<TcpStream>) -> Vec<u8> {
        let mut size_line = String::new();
        r.read_line(&mut size_line).unwrap();
        let size = usize::from_str_radix(size_line.trim(), 16).unwrap_or(0);
        let mut buf = vec![0u8; size];
        if size > 0 {
            r.read_exact(&mut buf).unwrap();
        }
        let mut crlf = [0u8; 2];
        if size > 0 {
            let _ = r.read_exact(&mut crlf);
        } else {
            // terminating chunk is "0\r\n\r\n"; one CRLF already consumed as the size line end
            let mut extra = String::new();
            let _ = r.read_line(&mut extra);
        }
        buf
    }

    fn next_ndjson(r: &mut BufReader<TcpStream>, carry: &mut Vec<u8>) -> String {
        loop {
            if let Some(i) = carry.iter().position(|b| *b == b'\n') {
                let line = carry.drain(..=i).collect::<Vec<u8>>();
                return String::from_utf8_lossy(&line).trim().to_string();
            }
            let chunk = read_chunk(r);
            if chunk.is_empty() {
                return String::new();
            }
            carry.extend_from_slice(&chunk);
        }
    }

    fn post(addr: std::net::SocketAddr, target: &str, body: &str) -> TcpStream {
        let mut stream = TcpStream::connect(addr).unwrap();
        let req = format!(
            "POST {target} HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        );
        stream.write_all(req.as_bytes()).unwrap();
        stream
    }

    #[test]
    fn exec_buffered_stdin_json() {
        let addr = serve_threaded();
        let mut stream = post(addr, "/v1/exec", r#"{"cmd":["/bin/sh","-c","cat"],"stdin":"xyz"}"#);
        let mut buf = Vec::new();
        stream.read_to_end(&mut buf).unwrap();
        let resp = String::from_utf8_lossy(&buf);
        assert!(resp.contains("\"stdout\":\"xyz\""), "{resp}");
        assert!(resp.contains("\"exit_code\":0"), "{resp}");
        assert!(!resp.to_ascii_lowercase().contains("ndjson"), "{resp}");
    }

    #[test]
    fn exec_stream_stdin_pipe_and_pty() {
        let addr = serve_threaded();
        for (label, body) in [
            ("pipe", r#"{"cmd":["/bin/sh","-c","cat"],"stdin_stream":true}"#),
            ("pty", r#"{"cmd":["/bin/sh","-c","cat"],"pty":true,"rows":24,"cols":80}"#),
        ] {
            let stream = post(addr, "/v1/exec?stream=1", body);
            let mut r = BufReader::new(stream);
            read_headers(&mut r);
            let mut carry = Vec::new();
            let ready = next_ndjson(&mut r, &mut carry);
            assert!(ready.contains("\"type\":\"ready\""), "{label} ready={ready}");
            let exec_id = ready.split("exec_id\":\"").nth(1).unwrap().split('"').next().unwrap().to_string();
            let payload = format!(r#"{{"exec_id":"{exec_id}","data":"hello-{label}\n","close":true}}"#);
            let mut stdin_stream = post(addr, "/v1/exec/stdin", &payload);
            let mut ack = Vec::new();
            stdin_stream.read_to_end(&mut ack).unwrap();
            let ack_s = String::from_utf8_lossy(&ack);
            assert!(ack_s.contains("200"), "{label} ack={ack_s}");
            let mut saw = String::new();
            for _ in 0..20 {
                let line = next_ndjson(&mut r, &mut carry);
                if line.is_empty() {
                    break;
                }
                saw.push_str(&line);
                if line.contains("\"type\":\"exit\"") {
                    break;
                }
            }
            assert!(saw.contains(&format!("hello-{label}")), "{label} saw={saw}");
            assert!(saw.contains("\"exit_code\":0"), "{label} saw={saw}");
        }
    }

    fn buffered(cmd: &[&str], stdin: Option<&str>, pty: bool, timeout: Duration) -> ExecResponse {
        let req = ExecRequest {
            cmd: cmd.iter().map(|s| s.to_string()).collect(),
            env: None,
            cwd: None,
            pty,
            rows: 0,
            cols: 0,
            stdin: stdin.map(str::to_string),
            stdin_stream: false,
        };
        run_exec(&req, timeout).unwrap()
    }

    #[test]
    fn buffered_exec_returns_stdout_larger_than_a_pipe() {
        let started = Instant::now();
        let resp = buffered(
            &["/bin/sh", "-c", "head -c 300000 /dev/zero | tr '\\000' a"],
            None,
            false,
            Duration::from_secs(20),
        );
        assert_eq!(resp.exit_code, 0, "stderr={}", resp.stderr);
        assert_eq!(resp.stdout.len(), 300_000);
        assert!(resp.stdout.bytes().all(|b| b == b'a'));
        assert!(started.elapsed() < Duration::from_secs(10), "took {:?}", started.elapsed());
    }

    #[test]
    fn buffered_exec_returns_stderr_larger_than_a_pipe() {
        let resp = buffered(
            &["/bin/sh", "-c", "head -c 300000 /dev/zero | tr '\\000' e 1>&2"],
            None,
            false,
            Duration::from_secs(20),
        );
        assert_eq!(resp.exit_code, 0);
        assert_eq!(resp.stderr.len(), 300_000);
        assert!(resp.stdout.is_empty(), "stdout={:?}", resp.stdout);
    }

    #[test]
    fn buffered_exec_echoes_stdin_larger_than_a_pipe() {
        let input = "x".repeat(300_000);
        let resp = buffered(&["/bin/sh", "-c", "cat"], Some(&input), false, Duration::from_secs(20));
        assert_eq!(resp.exit_code, 0, "stderr={}", resp.stderr);
        assert_eq!(resp.stdout.len(), input.len());
        assert!(resp.stdout == input);
    }

    #[test]
    fn buffered_exec_timeout_keeps_partial_output() {
        let resp = buffered(
            &["/bin/sh", "-c", "echo started; exec sleep 30"],
            None,
            false,
            Duration::from_millis(500),
        );
        assert_eq!(resp.exit_code, 124);
        assert!(resp.stdout.contains("started"), "stdout={:?}", resp.stdout);
        assert!(resp.stderr.contains("exec timed out"), "stderr={:?}", resp.stderr);
    }

    #[test]
    fn buffered_exec_does_not_wait_for_background_jobs() {
        let started = Instant::now();
        let resp = buffered(&["/bin/sh", "-c", "sleep 30 & echo done"], None, false, Duration::from_secs(20));
        assert_eq!(resp.exit_code, 0);
        assert!(resp.stdout.contains("done"), "stdout={:?}", resp.stdout);
        assert!(started.elapsed() < Duration::from_secs(5), "took {:?}", started.elapsed());
    }

    #[test]
    fn buffered_pty_exec_with_stdin_larger_than_the_tty_buffers() {
        let mut input = String::new();
        for i in 0..2000 {
            input.push_str(&format!("line{i:04}-{}\n", "y".repeat(40)));
        }
        let resp = buffered(&["/bin/sh", "-c", "cat"], Some(&input), true, Duration::from_secs(20));
        assert_eq!(resp.exit_code, 0, "stderr={:?}", resp.stderr);
        assert!(resp.stdout.contains("line1999"));
    }

    #[test]
    fn bad_request_line() {
        let cur = Cursor::new(Vec::<u8>::new());
        // Empty body after drop — just ensure empty request is ok.
        let empty = Cursor::new(Vec::<u8>::new());
        handle_connection(empty, Duration::from_secs(1)).unwrap();
        let _ = cur;
    }
}
