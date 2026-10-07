//! HTTP over TCP (guest listen on the TAP address for lab use; the productive CH
//! path uses vsock).

use crate::http_serve;
use crate::peer::{self, TcpBind};
use std::io;
use std::net::TcpListener;
use std::thread;

/// Serves on `bind.addr`, answering only `bind.peer` when set (the host's end of
/// the TAP): a process inside the guest that connects to the guest's own address
/// comes from that address and is dropped.
pub fn serve(bind: &TcpBind, limits: http_serve::ExecLimits) -> io::Result<()> {
    let listener = TcpListener::bind(bind.addr)?;
    match bind.peer {
        Some(peer) => println!("listening on tcp://{} (HTTP JSON, peer {peer} only)", bind.addr),
        None => println!("listening on tcp://{} (HTTP JSON, any peer)", bind.addr),
    }

    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                if let Ok(from) = stream.peer_addr() {
                    if !peer::tcp_peer_allowed(from.ip(), bind.peer) {
                        eprintln!("rejected a connection from {from}");
                        continue;
                    }
                }
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
