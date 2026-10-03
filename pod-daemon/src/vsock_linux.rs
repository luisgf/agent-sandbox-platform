//! Minimal AF_VSOCK listener/stream via libc syscalls (no external vsock crate).
//! Guest binds VMADDR_CID_ANY:port so Cloud Hypervisor CONNECT reaches us.

use std::io::{self, Read, Write};
use std::mem;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd};
use std::thread;
use std::time::Duration;

const AF_VSOCK: i32 = 40;
const SOCK_STREAM: i32 = 1;
const VMADDR_CID_ANY: u32 = 0xFFFF_FFFF;

#[repr(C)]
struct SockaddrVM {
    svm_family: u16,
    svm_reserved1: u16,
    svm_port: u32,
    svm_cid: u32,
    svm_zero: [u8; 4],
}

extern "C" {
    fn socket(domain: i32, ty: i32, protocol: i32) -> i32;
    fn bind(sockfd: i32, addr: *const u8, addrlen: u32) -> i32;
    fn listen(sockfd: i32, backlog: i32) -> i32;
    fn accept(sockfd: i32, addr: *mut u8, addrlen: *mut u32) -> i32;
    fn connect(sockfd: i32, addr: *const u8, addrlen: u32) -> i32;
    fn read(fd: i32, buf: *mut u8, count: usize) -> isize;
    fn write(fd: i32, buf: *const u8, count: usize) -> isize;
}

pub struct VsockListener {
    fd: OwnedFd,
    #[allow(dead_code)]
    port: u32,
}

pub struct VsockStream {
    fd: OwnedFd,
}

impl VsockListener {
    pub fn bind(port: u32) -> io::Result<Self> {
        let fd = unsafe { socket(AF_VSOCK, SOCK_STREAM, 0) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let owned = unsafe { OwnedFd::from_raw_fd(fd) };
        let addr = SockaddrVM {
            svm_family: AF_VSOCK as u16,
            svm_reserved1: 0,
            svm_port: port,
            svm_cid: VMADDR_CID_ANY,
            svm_zero: [0; 4],
        };
        let rc = unsafe {
            bind(
                owned.as_raw_fd(),
                &addr as *const _ as *const u8,
                mem::size_of::<SockaddrVM>() as u32,
            )
        };
        if rc < 0 {
            return Err(io::Error::last_os_error());
        }
        let rc = unsafe { listen(owned.as_raw_fd(), 128) };
        if rc < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(Self { fd: owned, port })
    }

    #[allow(dead_code)]
    pub fn port(&self) -> u32 {
        self.port
    }

    pub fn accept(&self) -> io::Result<VsockStream> {
        let mut addr = SockaddrVM {
            svm_family: 0,
            svm_reserved1: 0,
            svm_port: 0,
            svm_cid: 0,
            svm_zero: [0; 4],
        };
        let mut len = mem::size_of::<SockaddrVM>() as u32;
        let fd = unsafe {
            accept(
                self.fd.as_raw_fd(),
                &mut addr as *mut _ as *mut u8,
                &mut len,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(VsockStream {
            fd: unsafe { OwnedFd::from_raw_fd(fd) },
        })
    }

}

impl VsockStream {
    /// Dial host/hypervisor (typically CID 2) on `port` (e.g. 26501 SSH agent).
    pub fn connect(cid: u32, port: u32) -> io::Result<Self> {
        let fd = unsafe { socket(AF_VSOCK, SOCK_STREAM, 0) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let owned = unsafe { OwnedFd::from_raw_fd(fd) };
        let addr = SockaddrVM {
            svm_family: AF_VSOCK as u16,
            svm_reserved1: 0,
            svm_port: port,
            svm_cid: cid,
            svm_zero: [0; 4],
        };
        let rc = unsafe {
            connect(
                owned.as_raw_fd(),
                &addr as *const _ as *const u8,
                mem::size_of::<SockaddrVM>() as u32,
            )
        };
        if rc < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(Self { fd: owned })
    }

    #[allow(dead_code)]
    pub fn try_clone(&self) -> io::Result<Self> {
        let cloned = self.fd.try_clone()?;
        Ok(Self { fd: cloned })
    }

    #[allow(dead_code)]
    fn as_raw(&self) -> RawFd {
        self.fd.as_raw_fd()
    }
}

impl Read for VsockStream {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let n = unsafe { read(self.fd.as_raw_fd(), buf.as_mut_ptr(), buf.len()) };
        if n < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}

impl Write for VsockStream {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let n = unsafe { write(self.fd.as_raw_fd(), buf.as_ptr(), buf.len()) };
        if n < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

pub fn serve(port: u32, exec_timeout: Duration) -> io::Result<()> {
    let listener = VsockListener::bind(port)?;
    println!("listening on vsock://*:{port} (HTTP JSON, AF_VSOCK)");
    loop {
        match listener.accept() {
            Ok(stream) => {
                let timeout = exec_timeout;
                thread::spawn(move || {
                    if let Err(err) = crate::http_serve::handle_connection(stream, timeout) {
                        eprintln!("connection error: {err}");
                    }
                });
            }
            Err(error) => eprintln!("accept error: {error}"),
        }
    }
}
