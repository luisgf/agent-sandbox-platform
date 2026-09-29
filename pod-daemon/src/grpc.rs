//! Transport-independent service contracts for the MVP.
//!
//! The protobuf file is the wire-format source of truth. A later build step
//! will generate tonic types after vsock transport and authentication are set.

#![allow(dead_code)]

use std::io;

pub trait ExecService {
    fn exec(&self, sandbox_command: &[String]) -> io::Result<i32>;
}

pub trait FileService {
    fn read_file(&self, path: &str) -> io::Result<Vec<u8>>;
    fn write_file(&self, path: &str, data: &[u8], mode: u32) -> io::Result<()>;
}

pub trait MetricsService {
    fn snapshot(&self) -> io::Result<MetricsSnapshot>;
}

#[derive(Debug, Default)]
pub struct MetricsSnapshot {
    pub cpu_millis: u64,
    pub memory_bytes: u64,
    pub process_count: u32,
}
