//! Linux pseudoterminal. The guest runs the requested command with the slave
//! as stdin/stdout/stderr. The master stays in pod-daemon.

use std::collections::HashMap;
use std::fs::File;
use std::io;
use std::os::fd::{FromRawFd, IntoRawFd, RawFd};
use std::os::unix::fs::OpenOptionsExt;
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
    // c_char is i8 on x86_64 and u8 on aarch64: spell the type, not the sign.
    let mut name = [0 as libc::c_char; 128];
    if unsafe { libc::ptsname_r(master_fd, name.as_mut_ptr(), name.len()) } != 0 {
        let err = io::Error::last_os_error();
        unsafe { libc::close(master_fd) };
        return Err(err);
    }
    let cname = unsafe { std::ffi::CStr::from_ptr(name.as_ptr()) };
    let path = cname.to_string_lossy().into_owned();
    // O_NOCTTY: systemd starts pod-daemon as a session leader without a
    // controlling terminal, so a plain open makes the slave pod-daemon's own.
    // The child's TIOCSCTTY then fails, and when the slave closes the hangup
    // sends pod-daemon SIGHUP, which systemd counts as a clean exit.
    let slave = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .custom_flags(libc::O_NOCTTY)
        .open(&path)?;
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

#[cfg(all(test, target_os = "linux"))]
mod tests {
    use super::*;

    const CHILD_ENV: &str = "POD_DAEMON_PTY_TEST_SESSION_LEADER";
    const TEST_NAME: &str = "pty::tests::pty_works_in_a_session_leader";

    // systemd runs pod-daemon as a session leader without a controlling
    // terminal. The test runs itself again in such a process: the slave must
    // not become its controlling terminal, a command must start on the PTY, and
    // closing the PTY afterwards must not SIGHUP the process.
    #[test]
    fn pty_works_in_a_session_leader() {
        if std::env::var_os(CHILD_ENV).is_some() {
            let (master, slave_fd) = open_pty(24, 80).expect("open_pty");
            if unsafe { libc::tcgetsid(slave_fd) } != -1 {
                eprintln!("the slave became this session leader's controlling terminal");
                std::process::exit(1);
            }
            unsafe { libc::close(slave_fd) };
            drop(master);
            let (mut child, master) = spawn(PtyCommand {
                cmd: vec!["/bin/sh".into(), "-c".into(), "exit 0".into()],
                cwd: None,
                env: None,
                rows: 24,
                cols: 80,
            })
            .expect("spawn on the pty");
            let status = child.wait().expect("wait for the pty child");
            drop(master);
            // A SIGHUP from the hangup would kill this process here.
            std::thread::sleep(std::time::Duration::from_millis(200));
            std::process::exit(if status.success() { 0 } else { 2 });
        }
        let mut cmd = Command::new(std::env::current_exe().expect("test binary"));
        cmd.args(["--exact", TEST_NAME, "--test-threads=1"]).env(CHILD_ENV, "1");
        unsafe {
            cmd.pre_exec(|| {
                if libc::setsid() < 0 {
                    return Err(io::Error::last_os_error());
                }
                Ok(())
            });
        }
        let status = cmd.status().expect("run the test again as a session leader");
        assert!(status.success(), "pty in a session leader: {status:?}");
    }
}
