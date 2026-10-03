//! Shared HTTP/1.1 JSON handler for unix / TCP / vsock transports.

use serde::{Deserialize, Serialize};
use std::io::{self, BufRead, BufReader, Read, Write};
use std::process::{Command, Stdio};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::thread;
use std::time::{Duration, Instant};

#[derive(Debug, Deserialize)]
struct ExecRequest {
    cmd: Vec<String>,
    #[serde(default)]
    env: Option<std::collections::HashMap<String, String>>,
    #[serde(default)]
    cwd: Option<String>,
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

fn spawn_command(req: &ExecRequest) -> io::Result<std::process::Child> {
    let program = &req.cmd[0];
    let args = &req.cmd[1..];
    let mut command = Command::new(program);
    command.args(args).stdout(Stdio::piped()).stderr(Stdio::piped());
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
    command.spawn()
}

/// NDJSON over chunked HTTP. Each line is `{"type":"stdout"|"stderr","data":"..."}`
/// and a final `{"type":"exit","exit_code":N}`. The buffered JSON path is unchanged.
fn run_exec_stream<S: Write>(out: &mut S, req: &ExecRequest, timeout: Duration) -> io::Result<()> {
    let mut child = match spawn_command(req) {
        Ok(c) => c,
        Err(e) => {
            return write_json(
                out,
                500,
                &ErrorBody {
                    error: e.to_string(),
                },
            );
        }
    };
    let stdout = child.stdout.take();
    let stderr = child.stderr.take();
    let (tx, rx) = mpsc::channel::<(&'static str, Vec<u8>)>();
    let tx_out = tx.clone();
    let tx_err = tx.clone();
    drop(tx);
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

    write_chunked_headers(out)?;
    let started = Instant::now();
    let mut killed = false;
    loop {
        match rx.recv_timeout(Duration::from_millis(30)) {
            Ok((kind, data)) => {
                write_stream_event(out, kind, &data)?;
            }
            Err(RecvTimeoutError::Timeout) => {
                if !killed && started.elapsed() > timeout {
                    let _ = child.kill();
                    killed = true;
                }
                if killed && started.elapsed() > timeout + Duration::from_secs(2) {
                    break;
                }
            }
            Err(RecvTimeoutError::Disconnected) => break,
        }
    }
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
    let program = &req.cmd[0];
    let args = &req.cmd[1..];
    let mut command = Command::new(program);
    command
        .args(args)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
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

    let mut child = command.spawn()?;
    let started = Instant::now();
    loop {
        match child.try_wait()? {
            Some(status) => {
                let stdout = read_pipe(child.stdout.take())?;
                let stderr = read_pipe(child.stderr.take())?;
                let exit_code = status.code().unwrap_or(128);
                return Ok(ExecResponse {
                    stdout,
                    stderr,
                    exit_code,
                });
            }
            None => {
                if started.elapsed() > timeout {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Ok(ExecResponse {
                        stdout: String::new(),
                        stderr: format!("exec timed out after {}s", timeout.as_secs()),
                        exit_code: 124,
                    });
                }
                std::thread::sleep(Duration::from_millis(20));
            }
        }
    }
}

fn read_pipe<T: Read>(pipe: Option<T>) -> io::Result<String> {
    let mut buf = String::new();
    if let Some(mut p) = pipe {
        p.read_to_string(&mut buf)?;
    }
    Ok(buf)
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

    #[test]
    fn bad_request_line() {
        let cur = Cursor::new(Vec::<u8>::new());
        // Empty body after drop — just ensure empty request is ok.
        let empty = Cursor::new(Vec::<u8>::new());
        handle_connection(empty, Duration::from_secs(1)).unwrap();
        let _ = cur;
    }
}
