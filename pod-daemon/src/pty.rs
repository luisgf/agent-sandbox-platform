//! Linux pseudoterminal. The guest runs the requested command with the slave
//! as stdin/stdout/stderr. The master stays in pod-daemon.

use std::collections::HashMap;
use std::fs::File;
use std::io;
use std::os::fd::{FromRawFd, IntoRawFd, RawFd};
use std::os::unix::process::CommandExt;
use std::process::{Child, Command, Stdio};

pub fn open_pty(rows: u16, cols: u16) -> io::Result<(File, RawFd)> {
    let master_fd = unsafe { libc::posix_openpt(libc::O_RDWR | libc::O_NOCTTY | libc::O_CLOEXEC) };
    if master_fd < 0 {
        return Err(io::Error::last_os_error());
    }
    if unsafe { libc::grantpt(master_fd) } != 0 {
        let err = io::Error::last_os_error();
        unsafe { libc::close(master_fd) };
        return Err(err);
    }
    if unsafe { libc::unlockpt(master_fd) } != 0 {
        let err = io::Error::last_os_error();
        unsafe { libc::close(master_fd) };
        return Err(err);
    }
    let mut name = [0i8; 128];
    if unsafe { libc::ptsname_r(master_fd, name.as_mut_ptr(), name.len()) } != 0 {
        let err = io::Error::last_os_error();
        unsafe { libc::close(master_fd) };
        return Err(err);
    }
    let cname = unsafe { std::ffi::CStr::from_ptr(name.as_ptr()) };
    let path = cname.to_string_lossy().into_owned();
    let slave = std::fs::OpenOptions::new().read(true).write(true).open(&path)?;
    let slave_fd = slave.into_raw_fd();
    let rows = if rows == 0 { 24 } else { rows };
    let cols = if cols == 0 { 80 } else { cols };
    set_winsize(master_fd, rows, cols)?;
    let master = unsafe { File::from_raw_fd(master_fd) };
    Ok((master, slave_fd))
}

pub fn set_winsize(fd: RawFd, rows: u16, cols: u16) -> io::Result<()> {
    let ws = libc::winsize {
        ws_row: rows,
        ws_col: cols,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };
    let rc = unsafe { libc::ioctl(fd, libc::TIOCSWINSZ, &ws) };
    if rc < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

pub struct PtyCommand {
    pub cmd: Vec<String>,
    pub cwd: Option<String>,
    pub env: Option<HashMap<String, String>>,
    pub rows: u16,
    pub cols: u16,
}

/// Spawn `cmd` attached to a new PTY. The returned file is the master.
pub fn spawn(spec: PtyCommand) -> io::Result<(Child, File)> {
    if spec.cmd.is_empty() {
        return Err(io::Error::new(io::ErrorKind::InvalidInput, "cmd required"));
    }
    let (master, slave_fd) = open_pty(spec.rows, spec.cols)?;
    let mut command = Command::new(&spec.cmd[0]);
    command.args(&spec.cmd[1..]);
    command.stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null());
    if let Some(cwd) = spec.cwd.as_ref() {
        if !cwd.is_empty() {
            command.current_dir(cwd);
        }
    }
    if let Some(env) = spec.env.as_ref() {
        for (k, v) in env {
            command.env(k, v);
        }
    }
    // slave_fd is Copy (i32). pre_exec runs in the child; the parent closes
    // its copy after spawn so the master is the only side we keep.
    unsafe {
        command.pre_exec(move || {
            if libc::setsid() < 0 {
                return Err(io::Error::last_os_error());
            }
            if libc::ioctl(slave_fd, libc::TIOCSCTTY, 0) < 0 {
                return Err(io::Error::last_os_error());
            }
            for stdfd in 0..3 {
                if libc::dup2(slave_fd, stdfd) < 0 {
                    return Err(io::Error::last_os_error());
                }
            }
            if slave_fd > 2 {
                libc::close(slave_fd);
            }
            Ok(())
        });
    }
    match command.spawn() {
        Ok(child) => {
            unsafe { libc::close(slave_fd) };
            Ok((child, master))
        }
        Err(err) => {
            unsafe { libc::close(slave_fd) };
            Err(err)
        }
    }
}
