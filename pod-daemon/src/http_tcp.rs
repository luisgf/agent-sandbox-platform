//! HTTP over TCP (guest listen 0.0.0.0:26500 for TAP/lab; productive CH path uses vsock).

use crate::http_serve;
use std::io;
use std::net::TcpListener;
use std::thread;

pub fn serve(addr: &str, limits: http_serve::ExecLimits) -> io::Result<()> {
    let listener = TcpListener::bind(addr)?;
    println!("listening on tcp://{} (HTTP JSON)", addr);

    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                thread::spawn(move || {
                    if let Err(err) = http_serve::handle_connection(stream, limits) {
                        eprintln!("connection error: {err}");
                    }
                });
            }
            Err(error) => eprintln!("accept error: {error}"),
        }
    }
    Ok(())
}
