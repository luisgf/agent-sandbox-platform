//! Shared HTTP/1.1 JSON handler for unix / TCP / vsock transports.

use serde::{Deserialize, Serialize};
use std::fs::File;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::os::fd::{AsRawFd, RawFd};
use std::os::unix::process::CommandExt;
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

/// Time limits of the exec endpoints.
#[derive(Clone, Copy, Debug)]
pub struct ExecLimits {
    /// Kills a buffered exec (`POST /v1/exec` without `?stream=1`) after this long.
    pub buffered: Duration,
    /// Kills a streamed exec after this long without output and without stdin.
    /// None: a stream lasts as long as its command and its client.
    pub stream_idle: Option<Duration>,
}

/// Lets the stream path notice a client that closed the connection while the
/// command is silent: a write would only fail on the next output, and an
/// interactive shell can stay silent for hours.
pub trait PeerState {
    fn peer_closed(&self) -> bool {
        false
    }
}

impl PeerState for std::os::unix::net::UnixStream {
    fn peer_closed(&self) -> bool {
        fd_peer_closed(self.as_raw_fd())
    }
}

impl PeerState for std::net::TcpStream {
    fn peer_closed(&self) -> bool {
        fd_peer_closed(self.as_raw_fd())
    }
}

/// Linux reports POLLRDHUP once the peer closed the connection or shut down
/// its side (TCP, Unix and AF_VSOCK alike).
#[cfg(target_os = "linux")]
pub fn fd_peer_closed(fd: RawFd) -> bool {
    let mut pfd = libc::pollfd {
        fd,
        events: libc::POLLRDHUP,
        revents: 0,
    };
    let n = unsafe { libc::poll(&mut pfd, 1, 0) };
    n > 0 && pfd.revents & (libc::POLLRDHUP | libc::POLLHUP | libc::POLLERR) != 0
}

#[cfg(not(target_os = "linux"))]
pub fn fd_peer_closed(_fd: RawFd) -> bool {
    false
}

/// Kills the command and the processes it started: non-PTY commands get their
/// own process group, PTY commands lead their own session (and group).
fn kill_tree(child: &mut Child) {
    let pid = child.id() as libc::pid_t;
    if pid > 0 {
        unsafe {
            libc::kill(-pid, libc::SIGKILL);
        }
    }
    let _ = child.kill();
}

/// One parsed HTTP/1.1 request.
struct Request {
    method: String,
    path: String,
    body: Vec<u8>,
    /// The client asked to close the connection after the response
    /// (`Connection: close`, or HTTP/1.0 without keep-alive).
    close: bool,
}

/// Reads the next request on the connection. None: the client closed the
/// connection between requests. A malformed request line comes back with an
/// empty method.
fn read_request<R: BufRead>(r: &mut R) -> io::Result<Option<Request>> {
    let mut request_line = String::new();
    loop {
        request_line.clear();
        if r.read_line(&mut request_line)? == 0 {
            return Ok(None);
        }
        // An empty line between keep-alive requests is tolerated (RFC 9112 §2.2).
        if !request_line.trim().is_empty() {
            break;
        }
    }
    let parts: Vec<&str> = request_line.split_whitespace().collect();
    if parts.len() < 2 {
        return Ok(Some(Request {
            method: String::new(),
            path: String::new(),
            body: Vec::new(),
            close: true,
        }));
    }
    let method = parts[0].to_string();
    let path = parts[1].to_string();
    let http10 = parts.get(2).is_some_and(|v| v.eq_ignore_ascii_case("HTTP/1.0"));
    let mut content_length = 0usize;
    let (mut conn_close, mut conn_keep_alive) = (false, false);
    loop {
        let mut line = String::new();
        if r.read_line(&mut line)? == 0 || line == "\r\n" || line == "\n" {
            break;
        }
        let lower = line.to_ascii_lowercase();
        if let Some(rest) = lower.strip_prefix("content-length:") {
            content_length = rest.trim().parse().unwrap_or(0);
        } else if let Some(rest) = lower.strip_prefix("connection:") {
            for token in rest.split(',') {
                match token.trim() {
                    "close" => conn_close = true,
                    "keep-alive" => conn_keep_alive = true,
                    _ => {}
                }
            }
        }
    }
    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        r.read_exact(&mut body)?;
    }
    Ok(Some(Request {
        method,
        path,
        body,
        close: conn_close || (http10 && !conn_keep_alive),
    }))
}

/// Serves HTTP/1.1 on any stream (Unix, TCP, or AF_VSOCK). Buffered requests
/// keep the connection open for the next one unless the client asks to close
/// it, so the node agent does not reconnect (and repeat the hybrid vsock
/// CONNECT) for every call. A streamed exec always ends the connection: its
/// close is how both sides know the stream is over, and how pod-daemon notices
/// a client that went away.
pub fn handle_connection<S: Read + Write + PeerState>(stream: S, limits: ExecLimits) -> io::Result<()> {
    let mut conn = BufReader::new(stream);
    loop {
        let Some(req) = read_request(&mut conn)? else {
            return Ok(());
        };
        let (path_only, stream_exec) = path_and_stream(&req.path);
        if req.method == "POST" && path_only == "/v1/exec" && stream_exec {
            return match parse_exec(&req.body) {
                Err((status, body)) => write_bytes(conn.get_mut(), status, &body, false),
                Ok(exec) => run_exec_stream(conn.get_mut(), &exec, limits.stream_idle),
            };
        }
        let (status, body) = respond_buffered(&req.method, path_only, &req.body, limits.buffered);
        let keep_alive = !req.close;
        write_bytes(conn.get_mut(), status, &body, keep_alive)?;
        if !keep_alive {
            return Ok(());
        }
    }
}

/// Answers every request but a streamed exec.
fn respond_buffered(method: &str, path: &str, body: &[u8], timeout: Duration) -> (u16, Vec<u8>) {
    match (method, path) {
        ("", _) => (400, br#"{"error":"bad request line"}"#.to_vec()),
        ("GET", "/healthz") => (200, br#"{"status":"ok"}"#.to_vec()),
        ("POST", "/v1/exec/stdin") => stdin_response(body),
        ("POST", "/v1/exec") => match parse_exec(body) {
            Err(resp) => resp,
            Ok(req) => match run_exec(&req, timeout) {
                Ok(out) => json_body(200, &out),
                Err(e) => json_body(500, &ErrorBody { error: e.to_string() }),
            },
        },
        _ => (404, br#"{"error":"not found"}"#.to_vec()),
    }
}

/// Parses an exec body, or returns the 400 to answer.
fn parse_exec(body: &[u8]) -> Result<ExecRequest, (u16, Vec<u8>)> {
    let req: ExecRequest = serde_json::from_slice(body).map_err(|e| {
        json_body(
            400,
            &ErrorBody {
                error: format!("invalid JSON: {e}"),
            },
        )
    })?;
    if req.cmd.is_empty() {
        return Err(json_body(
            400,
            &ErrorBody {
                error: "cmd required".into(),
            },
        ));
    }
    Ok(req)
}

fn json_body<T: Serialize>(status: u16, body: &T) -> (u16, Vec<u8>) {
    match serde_json::to_vec(body) {
        Ok(b) => (status, b),
        Err(e) => (
            500,
            serde_json::json!({ "error": format!("encode response: {e}") })
                .to_string()
                .into_bytes(),
        ),
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
        .stderr(Stdio::piped())
        .process_group(0);
    apply_cwd_env(&mut command, req);
    command.spawn()
}

fn spawn_with_stdin_pipe(req: &ExecRequest) -> io::Result<std::process::Child> {
    let mut command = Command::new(&req.cmd[0]);
    command
        .args(&req.cmd[1..])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
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
            kill_tree(&mut child);
            let _ = child.wait();
        }
    }
}

/// NDJSON over chunked HTTP. Each line is `{"type":"stdout"|"stderr","data":"..."}`
/// and a final `{"type":"exit","exit_code":N}`. When `pty` or `stdin_stream` is
/// set, the first line is `{"type":"ready","exec_id":"..."}` and stdin arrives
/// on POST /v1/exec/stdin. The buffered JSON path is unchanged.
///
/// A stream has no overall deadline: an interactive shell lasts as long as its
/// user. It ends when the command exits, when the client closes the connection
/// (the command is killed: nobody is left to read it), or, with `idle` set,
/// after that long without output and without stdin (exit code 124).
fn run_exec_stream<S: Write + PeerState>(out: &mut S, req: &ExecRequest, idle: Option<Duration>) -> io::Result<()> {
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
    let mut last_output = Instant::now();
    let mut killed_at: Option<Instant> = None;
    loop {
        match rx.recv_timeout(Duration::from_millis(30)) {
            Ok((kind, data)) => {
                last_output = Instant::now();
                write_stream_event(out, kind, &data)?;
            }
            Err(RecvTimeoutError::Timeout) => {
                if out.peer_closed() {
                    // The client went away while the command was silent.
                    // ChildGuard kills it and its group on the way out.
                    return Ok(());
                }
                match (killed_at, idle) {
                    (Some(at), _) if at.elapsed() > Duration::from_secs(2) => break,
                    (Some(_), _) | (None, None) => {}
                    (None, Some(idle)) => {
                        let input = session.0.as_deref().and_then(exec_session::last_input);
                        let last = input.map_or(last_output, |i| i.max(last_output));
                        if last.elapsed() > idle {
                            if let Some(child) = guard.0.as_mut() {
                                kill_tree(child);
                            }
                            killed_at = Some(Instant::now());
                        }
                    }
                }
            }
            Err(RecvTimeoutError::Disconnected) => break,
        }
    }
    let mut child = guard.0.take().unwrap();
    let status = child.wait()?;
    let killed = killed_at.is_some();
    let code = if killed {
        124
    } else {
        status.code().unwrap_or(128)
    };
    if let (true, Some(idle)) = (killed, idle) {
        let msg = format!("exec stopped after {idle:?} without output or input");
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

fn stdin_response(body: &[u8]) -> (u16, Vec<u8>) {
    let req: StdinRequest = match serde_json::from_slice(body) {
        Ok(r) => r,
        Err(e) => {
            return json_body(
                400,
                &ErrorBody {
                    error: format!("invalid JSON: {e}"),
                },
            )
        }
    };
    if req.exec_id.is_empty() {
        return json_body(
            400,
            &ErrorBody {
                error: "exec_id required".into(),
            },
        );
    }
    match exec_session::write(&req.exec_id, req.data.as_bytes(), req.close, req.rows, req.cols) {
        Ok(()) => json_body(200, &serde_json::json!({"ok": true})),
        Err(e) if e.kind() == io::ErrorKind::NotFound => json_body(404, &ErrorBody { error: e.to_string() }),
        Err(e) => json_body(500, &ErrorBody { error: e.to_string() }),
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
    kill_tree(&mut child);
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
                    kill_tree(&mut child);
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

/// Writes a JSON response and ends the connection (the stream path's errors).
fn write_json<S: Write, T: Serialize>(stream: &mut S, status: u16, body: &T) -> io::Result<()> {
    let (status, payload) = json_body(status, body);
    write_bytes(stream, status, &payload, false)
}

fn write_bytes<S: Write>(stream: &mut S, status: u16, body: &[u8], keep_alive: bool) -> io::Result<()> {
    let reason = match status {
        200 => "OK",
        400 => "Bad Request",
        404 => "Not Found",
        500 => "Internal Server Error",
        _ => "Error",
    };
    let connection = if keep_alive { "keep-alive" } else { "close" };
    let header = format!(
        "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: {connection}\r\n\r\n",
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

    impl PeerState for Cursor<Vec<u8>> {}

    fn limits(buffered_secs: u64) -> ExecLimits {
        ExecLimits {
            buffered: Duration::from_secs(buffered_secs),
            stream_idle: None,
        }
    }

    #[test]
    fn healthz_over_tcp() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let (ready_tx, ready_rx) = mpsc::channel();
        thread::spawn(move || {
            ready_tx.send(()).ok();
            let (stream, _) = listener.accept().unwrap();
            handle_connection(stream, limits(5)).unwrap();
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
            handle_connection(stream, limits(5)).unwrap();
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
        serve_with(limits(5))
    }

    fn serve_with(limits: ExecLimits) -> std::net::SocketAddr {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        thread::spawn(move || {
            for stream in listener.incoming().flatten() {
                thread::spawn(move || {
                    let _ = handle_connection(stream, limits);
                });
            }
        });
        addr
    }

    /// Reads NDJSON lines until the exit event (or the end of the body).
    fn read_until_exit(r: &mut BufReader<TcpStream>, carry: &mut Vec<u8>) -> String {
        let mut saw = String::new();
        loop {
            let line = next_ndjson(r, carry);
            if line.is_empty() {
                return saw;
            }
            saw.push_str(&line);
            saw.push('\n');
            if line.contains("\"type\":\"exit\"") {
                return saw;
            }
        }
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

    // The buffered exec timeout does not apply to a stream.
    #[test]
    fn stream_exec_outlives_the_buffered_timeout() {
        let addr = serve_with(ExecLimits {
            buffered: Duration::from_millis(300),
            stream_idle: None,
        });
        let stream = post(addr, "/v1/exec?stream=1", r#"{"cmd":["/bin/sh","-c","sleep 1; echo done"]}"#);
        let mut r = BufReader::new(stream);
        read_headers(&mut r);
        let saw = read_until_exit(&mut r, &mut Vec::new());
        assert!(saw.contains("done"), "{saw}");
        assert!(saw.contains("\"exit_code\":0"), "{saw}");
    }

    // A client that closes the connection while the command is silent takes
    // the command down with it.
    #[test]
    fn closing_the_client_kills_a_silent_stream() {
        let addr = serve_with(limits(30));
        let stream = post(addr, "/v1/exec?stream=1", r#"{"cmd":["/bin/sh","-c","echo $$; exec sleep 30"]}"#);
        let mut r = BufReader::new(stream);
        read_headers(&mut r);
        let line = next_ndjson(&mut r, &mut Vec::new());
        let ev: serde_json::Value = serde_json::from_str(&line).expect("stdout event");
        let pid: i32 = ev["data"].as_str().unwrap().trim().parse().expect("pid");
        drop(r);
        let deadline = Instant::now() + Duration::from_secs(5);
        while unsafe { libc::kill(pid, 0) } == 0 {
            assert!(Instant::now() < deadline, "pid {pid} still alive after the client left");
            thread::sleep(Duration::from_millis(50));
        }
    }

    #[test]
    fn stream_idle_timeout_stops_a_silent_command_only() {
        let addr = serve_with(ExecLimits {
            buffered: Duration::from_secs(30),
            stream_idle: Some(Duration::from_millis(500)),
        });
        let started = Instant::now();
        let stream = post(addr, "/v1/exec?stream=1", r#"{"cmd":["/bin/sh","-c","exec sleep 10"]}"#);
        let mut r = BufReader::new(stream);
        read_headers(&mut r);
        let saw = read_until_exit(&mut r, &mut Vec::new());
        assert!(saw.contains("\"exit_code\":124"), "{saw}");
        assert!(saw.contains("without output or input"), "{saw}");
        assert!(started.elapsed() < Duration::from_secs(5), "took {:?}", started.elapsed());

        let chatty = r#"{"cmd":["/bin/sh","-c","for i in 1 2 3 4 5 6; do echo tick$i; sleep 0.2; done"]}"#;
        let stream = post(addr, "/v1/exec?stream=1", chatty);
        let mut r = BufReader::new(stream);
        read_headers(&mut r);
        let saw = read_until_exit(&mut r, &mut Vec::new());
        assert!(saw.contains("tick6") && saw.contains("\"exit_code\":0"), "{saw}");
    }

    // Typing into a silent command is activity too.
    #[test]
    fn stream_idle_timeout_counts_stdin() {
        let addr = serve_with(ExecLimits {
            buffered: Duration::from_secs(30),
            stream_idle: Some(Duration::from_millis(500)),
        });
        let stream = post(
            addr,
            "/v1/exec?stream=1",
            r#"{"cmd":["/bin/sh","-c","cat >/dev/null; echo end"],"stdin_stream":true}"#,
        );
        let mut r = BufReader::new(stream);
        read_headers(&mut r);
        let mut carry = Vec::new();
        let ready = next_ndjson(&mut r, &mut carry);
        let exec_id = ready.split("exec_id\":\"").nth(1).unwrap().split('"').next().unwrap().to_string();
        for _ in 0..6 {
            let mut s = post(addr, "/v1/exec/stdin", &format!(r#"{{"exec_id":"{exec_id}","data":"x\n"}}"#));
            let mut ack = Vec::new();
            s.read_to_end(&mut ack).unwrap();
            thread::sleep(Duration::from_millis(200));
        }
        let mut s = post(addr, "/v1/exec/stdin", &format!(r#"{{"exec_id":"{exec_id}","close":true}}"#));
        let mut ack = Vec::new();
        s.read_to_end(&mut ack).unwrap();
        let saw = read_until_exit(&mut r, &mut carry);
        assert!(saw.contains("end") && saw.contains("\"exit_code\":0"), "{saw}");
    }

    /// Reads one Content-Length response off a persistent connection: the
    /// status line plus the Connection header, and the body.
    fn read_response(r: &mut BufReader<TcpStream>) -> (String, String) {
        let mut status = String::new();
        r.read_line(&mut status).unwrap();
        let (mut len, mut connection) = (0usize, String::new());
        loop {
            let mut line = String::new();
            r.read_line(&mut line).unwrap();
            if line == "\r\n" || line.is_empty() {
                break;
            }
            let lower = line.to_ascii_lowercase();
            if let Some(v) = lower.strip_prefix("content-length:") {
                len = v.trim().parse().unwrap();
            }
            if let Some(v) = lower.strip_prefix("connection:") {
                connection = v.trim().to_string();
            }
        }
        let mut body = vec![0u8; len];
        r.read_exact(&mut body).unwrap();
        (format!("{} | {connection}", status.trim()), String::from_utf8(body).unwrap())
    }

    #[test]
    fn keep_alive_serves_several_requests_on_one_connection() {
        let addr = serve_threaded();
        let stream = TcpStream::connect(addr).unwrap();
        let mut w = stream.try_clone().unwrap();
        let mut r = BufReader::new(stream);

        w.write_all(b"GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
        let (head, body) = read_response(&mut r);
        assert!(head.starts_with("HTTP/1.1 200") && head.ends_with("keep-alive"), "{head}");
        assert!(body.contains("ok"), "{body}");

        let exec = br#"{"cmd":["echo","again"]}"#;
        let req = format!("POST /v1/exec HTTP/1.1\r\nHost: x\r\nContent-Length: {}\r\n\r\n", exec.len());
        w.write_all(req.as_bytes()).unwrap();
        w.write_all(exec).unwrap();
        let (head, body) = read_response(&mut r);
        assert!(head.starts_with("HTTP/1.1 200"), "{head}");
        assert!(body.contains("again"), "{body}");

        w.write_all(b"GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n").unwrap();
        let (head, _) = read_response(&mut r);
        assert!(head.ends_with("close"), "{head}");
        let mut rest = Vec::new();
        assert_eq!(r.read_to_end(&mut rest).unwrap(), 0, "the server must close after Connection: close");
    }

    #[test]
    fn http10_without_keep_alive_closes() {
        let addr = serve_threaded();
        let mut stream = TcpStream::connect(addr).unwrap();
        stream.write_all(b"GET /healthz HTTP/1.0\r\n\r\n").unwrap();
        let mut buf = Vec::new();
        stream.read_to_end(&mut buf).unwrap();
        let resp = String::from_utf8_lossy(&buf).to_ascii_lowercase();
        assert!(resp.contains("200") && resp.contains("connection: close"), "{resp}");
    }

    #[test]
    fn bad_request_line() {
        let cur = Cursor::new(Vec::<u8>::new());
        // Empty body after drop — just ensure empty request is ok.
        let empty = Cursor::new(Vec::<u8>::new());
        handle_connection(empty, limits(1)).unwrap();
        let _ = cur;
    }
}
