//! In-guest exec sessions so a second HTTP request can write stdin / resize
//! while the stream response is still open.

use std::collections::HashMap;
use std::fs::File;
use std::io::{self, Read, Write};
use std::os::fd::AsRawFd;
use std::process::ChildStdin;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Instant;

pub enum StdinSlot {
    Pipe(Mutex<Option<ChildStdin>>),
    Pty(Mutex<File>),
}

struct Session {
    slot: StdinSlot,
    /// Last stdin write or resize: the stream's idle timeout counts input as
    /// activity, so a user typing into a silent command keeps it alive.
    last_input: Mutex<Instant>,
}

fn sessions() -> &'static Mutex<HashMap<String, Arc<Session>>> {
    static SESSIONS: OnceLock<Mutex<HashMap<String, Arc<Session>>>> = OnceLock::new();
    SESSIONS.get_or_init(|| Mutex::new(HashMap::new()))
}

pub fn register(slot: StdinSlot) -> String {
    let id = new_id();
    let session = Session {
        slot,
        last_input: Mutex::new(Instant::now()),
    };
    sessions()
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .insert(id.clone(), Arc::new(session));
    id
}

/// When the session last got stdin or a resize; None for an unknown id.
pub fn last_input(id: &str) -> Option<Instant> {
    let map = sessions().lock().unwrap_or_else(|p| p.into_inner());
    map.get(id)
        .map(|s| *s.last_input.lock().unwrap_or_else(|p| p.into_inner()))
}

pub fn remove(id: &str) {
    sessions()
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .remove(id);
}

pub fn write(id: &str, data: &[u8], close: bool, rows: u16, cols: u16) -> io::Result<()> {
    let session = {
        let map = sessions().lock().unwrap_or_else(|p| p.into_inner());
        map.get(id)
            .cloned()
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "unknown exec_id"))?
    };
    *session.last_input.lock().unwrap_or_else(|p| p.into_inner()) = Instant::now();
    match &session.slot {
        StdinSlot::Pipe(slot) => {
            let mut guard = slot.lock().unwrap_or_else(|p| p.into_inner());
            let Some(w) = guard.as_mut() else {
                return Err(io::Error::new(io::ErrorKind::BrokenPipe, "stdin closed"));
            };
            if !data.is_empty() {
                w.write_all(data)?;
                w.flush()?;
            }
            if close {
                *guard = None;
            }
            Ok(())
        }
        StdinSlot::Pty(slot) => {
            let mut master = slot.lock().unwrap_or_else(|p| p.into_inner());
            if rows > 0 && cols > 0 {
                let _ = crate::pty::set_winsize(master.as_raw_fd(), rows, cols);
            }
            if !data.is_empty() {
                master.write_all(data)?;
                master.flush()?;
            }
            if close {
                // Canonical PTY: EOT flushes a partial line and a second EOT
                // is EOF. This is not SIGHUP and it is not a raw-mode EOF.
                let _ = master.write_all(&[0x04, 0x04]);
                let _ = master.flush();
            }
            Ok(())
        }
    }
}

fn new_id() -> String {
    let mut bytes = [0u8; 8];
    if let Ok(mut f) = File::open("/dev/urandom") {
        let _ = f.read_exact(&mut bytes);
    }
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
