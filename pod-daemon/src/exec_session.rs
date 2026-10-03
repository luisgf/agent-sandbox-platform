//! In-guest exec sessions so a second HTTP request can write stdin / resize
//! while the stream response is still open.

use std::collections::HashMap;
use std::fs::File;
use std::io::{self, Read, Write};
use std::os::fd::AsRawFd;
use std::process::ChildStdin;
use std::sync::{Arc, Mutex, OnceLock};

pub enum StdinSlot {
    Pipe(Mutex<Option<ChildStdin>>),
    Pty(Mutex<File>),
}

fn sessions() -> &'static Mutex<HashMap<String, Arc<StdinSlot>>> {
    static SESSIONS: OnceLock<Mutex<HashMap<String, Arc<StdinSlot>>>> = OnceLock::new();
    SESSIONS.get_or_init(|| Mutex::new(HashMap::new()))
}

pub fn register(slot: StdinSlot) -> String {
    let id = new_id();
    sessions()
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .insert(id.clone(), Arc::new(slot));
    id
}

pub fn remove(id: &str) {
    sessions()
        .lock()
        .unwrap_or_else(|p| p.into_inner())
        .remove(id);
}

pub fn write(id: &str, data: &[u8], close: bool, rows: u16, cols: u16) -> io::Result<()> {
    let slot = {
        let map = sessions().lock().unwrap_or_else(|p| p.into_inner());
        map.get(id)
            .cloned()
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "unknown exec_id"))?
    };
    match slot.as_ref() {
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
