//! HTTP over TCP (guest listen 0.0.0.0:26500 for TAP/lab; productive CH path uses vsock).

use crate::http_serve;
use std::io;
use std::net::TcpListener;
use std::thread;
use std::time::Duration;

pub fn serve(addr: &str, exec_timeout: Duration) -> io::Result<()> {
    let listener = TcpListener::bind(addr)?;
    println!("listening on tcp://{} (HTTP JSON)", addr);

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
