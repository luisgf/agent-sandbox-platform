//! Who an exec runs as, and the limits it runs under.
//!
//! pod-daemon runs as root in the guest because it has to start commands as
//! other users, and as root when a caller asks for it. A command is not root
//! unless the request says `as_root`: it runs as the owner of the workspace, or
//! as the `sandboxd` account, with its own resource limits, in the exec cgroup.
//! Who may call the daemon at all is decided where it accepts connections (see
//! `peer`), so a process inside the guest cannot use it to become root.

use std::ffi::{CStr, CString};
use std::io;
use std::os::fd::{AsRawFd, OwnedFd};
use std::os::unix::ffi::OsStrExt;
use std::os::unix::fs::MetadataExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::Command;

/// An account in the guest's passwd.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Account {
    pub name: String,
    pub uid: u32,
    pub gid: u32,
    pub home: PathBuf,
}

/// What the policy needs to know about the guest's accounts and files. The real
/// implementation reads passwd and the filesystem; tests substitute their own.
pub trait Accounts {
    fn by_name(&self, name: &str) -> Option<Account>;
    fn by_uid(&self, uid: u32) -> Option<Account>;
    /// The owner (uid, gid) of path, or None when it cannot be read.
    fn owner_of(&self, path: &Path) -> Option<(u32, u32)>;
}

/// Accounts as the guest's libc sees them.
pub struct SystemAccounts;

fn passwd_account(pw: &libc::passwd) -> Account {
    let text = |p: *const libc::c_char| {
        if p.is_null() {
            String::new()
        } else {
            unsafe { CStr::from_ptr(p) }.to_string_lossy().into_owned()
        }
    };
    Account {
        name: text(pw.pw_name),
        uid: pw.pw_uid,
        gid: pw.pw_gid,
        home: PathBuf::from(text(pw.pw_dir)),
    }
}

/// Grows the buffer a `get*_r` call asks for, up to a sane bound.
fn lookup(call: impl Fn(*mut libc::passwd, *mut libc::c_char, usize, *mut *mut libc::passwd) -> libc::c_int) -> Option<Account> {
    let mut size = 1024;
    loop {
        let mut pw: libc::passwd = unsafe { std::mem::zeroed() };
        let mut buf = vec![0u8; size];
        let mut result: *mut libc::passwd = std::ptr::null_mut();
        let rc = call(&mut pw, buf.as_mut_ptr() as *mut libc::c_char, buf.len(), &mut result);
        if rc == libc::ERANGE && size < (1 << 20) {
            size *= 2;
            continue;
        }
        if rc != 0 || result.is_null() {
            return None;
        }
        return Some(passwd_account(&pw));
    }
}

impl Accounts for SystemAccounts {
    fn by_name(&self, name: &str) -> Option<Account> {
        let cname = CString::new(name).ok()?;
        lookup(|pw, buf, len, out| unsafe { libc::getpwnam_r(cname.as_ptr(), pw, buf, len, out) })
    }

    fn by_uid(&self, uid: u32) -> Option<Account> {
        lookup(|pw, buf, len, out| unsafe { libc::getpwuid_r(uid, pw, buf, len, out) })
    }

    fn owner_of(&self, path: &Path) -> Option<(u32, u32)> {
        let meta = std::fs::metadata(path).ok()?;
        Some((meta.uid(), meta.gid()))
    }
}

/// Resource limits of every exec. Zero means "do not set this one".
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Limits {
    /// RLIMIT_NPROC (per user) and, in the exec cgroup, pids.max (all execs).
    pub max_procs: u64,
    /// RLIMIT_NOFILE.
    pub max_open_files: u64,
    /// Allow core dumps. Off: RLIMIT_CORE is 0, so a crash does not write a copy
    /// of the process's memory (secrets included) next to the workspace.
    pub core_dumps: bool,
}

#[cfg(test)]
impl Limits {
    pub const NONE: Limits = Limits {
        max_procs: 0,
        max_open_files: 0,
        core_dumps: true,
    };
}

/// How execs are run.
#[derive(Clone, Debug)]
pub struct ExecPolicy {
    /// The account commands run as when the workspace has no non-root owner. None
    /// turns user switching off: commands run as whatever the daemon is.
    pub default_user: Option<String>,
    /// Where the host's workspace is mounted in the guest. virtiofs does not map
    /// ids, so a command runs as the owner of this directory and the files it
    /// writes keep the host user's uid. A root-owned workspace gives no owner.
    pub workspace_dir: PathBuf,
    pub limits: Limits,
    /// A cgroup (already created, with its limits) that every exec joins.
    pub cgroup: Option<PathBuf>,
}

#[cfg(test)]
impl ExecPolicy {
    /// The daemon's old behaviour: commands keep the daemon's identity and run
    /// unconfined.
    pub fn no_switch() -> ExecPolicy {
        ExecPolicy {
            default_user: None,
            workspace_dir: PathBuf::new(),
            limits: Limits::NONE,
            cgroup: None,
        }
    }

    /// A shared `no_switch` policy for callers that need a `&'static`.
    pub fn no_switch_static() -> &'static ExecPolicy {
        static P: std::sync::OnceLock<ExecPolicy> = std::sync::OnceLock::new();
        P.get_or_init(ExecPolicy::no_switch)
    }
}

/// The identity a command runs with.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RunAs {
    /// The daemon's own: root with `as_root`, or whatever the daemon is when it
    /// is not root or user switching is off.
    Inherit,
    /// Another account. Supplementary groups are cleared.
    User(Account),
}

/// Decides who a command runs as. Pure: everything it needs comes in.
///
/// An error is what to tell the caller. `Err(Denied)` is a request the daemon
/// refuses; `Err(Misconfigured)` is a guest that cannot run commands as the
/// account it was asked to.
pub fn resolve(policy: &ExecPolicy, accounts: &dyn Accounts, euid: u32, as_root: bool) -> Result<RunAs, ResolveError> {
    if euid != 0 {
        if as_root {
            return Err(ResolveError::Denied(format!(
                "as_root needs pod-daemon to run as root; it runs as uid {euid}"
            )));
        }
        return Ok(RunAs::Inherit);
    }
    let Some(default_user) = policy.default_user.as_deref() else {
        return Ok(RunAs::Inherit);
    };
    if as_root {
        return Ok(RunAs::Inherit);
    }
    if let Some((uid, gid)) = accounts.owner_of(&policy.workspace_dir) {
        if uid != 0 {
            let known = accounts.by_uid(uid);
            return Ok(RunAs::User(Account {
                name: known.as_ref().map(|a| a.name.clone()).unwrap_or_default(),
                uid,
                gid,
                home: known.map(|a| a.home).filter(|h| h.is_dir()).unwrap_or_else(|| PathBuf::from("/tmp")),
            }));
        }
    }
    match accounts.by_name(default_user) {
        Some(account) => Ok(RunAs::User(account)),
        None => Err(ResolveError::Misconfigured(format!(
            "the default exec user {default_user:?} does not exist in this guest; \
             run the command with as_root, or fix the image (--exec-user)"
        ))),
    }
}

#[derive(Debug, PartialEq, Eq)]
pub enum ResolveError {
    Denied(String),
    Misconfigured(String),
}

/// A request the daemon refuses on purpose. It is the error inside an
/// `io::Error` so a caller can tell it from the operating system's own EACCES
/// (a command that is not executable): the first is a 403, the second a failed
/// start.
#[derive(Debug)]
pub struct Refused(pub String);

impl std::fmt::Display for Refused {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for Refused {}

/// Whether `e` is a refusal by the daemon's policy.
pub fn is_refusal(e: &io::Error) -> bool {
    e.get_ref().is_some_and(|inner| inner.is::<Refused>())
}

impl From<ResolveError> for io::Error {
    fn from(e: ResolveError) -> io::Error {
        match e {
            ResolveError::Denied(m) => io::Error::new(io::ErrorKind::PermissionDenied, Refused(m)),
            ResolveError::Misconfigured(m) => io::Error::other(m),
        }
    }
}

#[derive(Clone, Copy, Debug)]
enum Rl {
    Nproc,
    Nofile,
    Core,
}

impl Rl {
    fn set(self, lim: &libc::rlimit) -> libc::c_int {
        match self {
            Rl::Nproc => unsafe { libc::setrlimit(libc::RLIMIT_NPROC, lim) },
            Rl::Nofile => unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, lim) },
            Rl::Core => unsafe { libc::setrlimit(libc::RLIMIT_CORE, lim) },
        }
    }

    fn get(self, lim: &mut libc::rlimit) -> libc::c_int {
        match self {
            Rl::Nproc => unsafe { libc::getrlimit(libc::RLIMIT_NPROC, lim) },
            Rl::Nofile => unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, lim) },
            Rl::Core => unsafe { libc::getrlimit(libc::RLIMIT_CORE, lim) },
        }
    }
}

/// A limit that can be applied without privilege: the soft and the hard value
/// are both the wanted one, or the current hard limit when that is lower, so the
/// command can neither raise it nor be told it asked for more than it may.
fn lowered(which: Rl, want: u64) -> io::Result<(Rl, libc::rlimit)> {
    let mut cur = libc::rlimit { rlim_cur: 0, rlim_max: 0 };
    if which.get(&mut cur) != 0 {
        return Err(io::Error::last_os_error());
    }
    let value = (want as libc::rlim_t).min(cur.rlim_max);
    Ok((which, libc::rlimit { rlim_cur: value, rlim_max: value }))
}

/// What a child does between fork and exec. Everything is prepared in the
/// parent: the closure only makes system calls (no allocation after fork).
struct ChildSetup {
    cgroup_procs: Option<OwnedFd>,
    limits: Vec<(Rl, libc::rlimit)>,
    ids: Option<(libc::gid_t, libc::uid_t)>,
    cwd: Option<CString>,
}

impl ChildSetup {
    /// Runs in the child. The order matters: join the cgroup and set limits while
    /// still privileged, drop to the target user, and only then change directory,
    /// so the directory has to be reachable by that user.
    fn run(&self) -> io::Result<()> {
        if let Some(fd) = &self.cgroup_procs {
            // Writing 0 to cgroup.procs moves the writing process.
            let n = unsafe { libc::write(fd.as_raw_fd(), b"0".as_ptr() as *const libc::c_void, 1) };
            if n != 1 {
                return Err(io::Error::last_os_error());
            }
        }
        for (which, lim) in &self.limits {
            if which.set(lim) != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        if let Some((gid, uid)) = self.ids {
            if unsafe { libc::setgroups(0, std::ptr::null()) } != 0 {
                return Err(io::Error::last_os_error());
            }
            if unsafe { libc::setgid(gid) } != 0 {
                return Err(io::Error::last_os_error());
            }
            if unsafe { libc::setuid(uid) } != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        if let Some(cwd) = &self.cwd {
            if unsafe { libc::chdir(cwd.as_ptr()) } != 0 {
                return Err(io::Error::last_os_error());
            }
        }
        Ok(())
    }
}

/// Everything decided about one exec, ready to put on a command.
pub struct Prepared {
    env: Vec<(&'static str, String)>,
    setup: ChildSetup,
}

impl Prepared {
    /// The identity's target ids, for the PTY slave.
    pub fn ids(&self) -> Option<(u32, u32)> {
        self.setup.ids.map(|(gid, uid)| (uid, gid))
    }

    /// Puts the identity, limits and cgroup on `command`. The environment this
    /// sets (HOME, USER, LOGNAME) comes first: the request's own env, applied
    /// after, wins. The working directory is changed in the child, after the user
    /// switch, so do not call `current_dir` as well.
    pub fn apply(self, command: &mut Command) {
        for (k, v) in &self.env {
            command.env(k, v);
        }
        let setup = self.setup;
        unsafe {
            command.pre_exec(move || setup.run());
        }
    }
}

/// Resolves the identity and gathers the limits and the cgroup for one exec.
pub fn prepare(
    policy: &ExecPolicy,
    accounts: &dyn Accounts,
    euid: u32,
    as_root: bool,
    cwd: Option<&str>,
) -> io::Result<Prepared> {
    let run_as = resolve(policy, accounts, euid, as_root)?;
    let mut limits = Vec::new();
    if policy.limits.max_procs > 0 {
        limits.push(lowered(Rl::Nproc, policy.limits.max_procs)?);
    }
    if policy.limits.max_open_files > 0 {
        limits.push(lowered(Rl::Nofile, policy.limits.max_open_files)?);
    }
    if !policy.limits.core_dumps {
        limits.push(lowered(Rl::Core, 0)?);
    }
    let cgroup_procs = match &policy.cgroup {
        Some(dir) => Some(OwnedFd::from(
            std::fs::OpenOptions::new().write(true).open(dir.join("cgroup.procs"))?,
        )),
        None => None,
    };
    let cwd = match cwd {
        Some(c) if !c.is_empty() => Some(
            CString::new(Path::new(c).as_os_str().as_bytes())
                .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "cwd contains a NUL byte"))?,
        ),
        _ => None,
    };
    let mut env = Vec::new();
    let ids = match &run_as {
        RunAs::Inherit => None,
        RunAs::User(account) => {
            env.push(("HOME", account.home.to_string_lossy().into_owned()));
            if !account.name.is_empty() {
                env.push(("USER", account.name.clone()));
                env.push(("LOGNAME", account.name.clone()));
            }
            Some((account.gid, account.uid))
        }
    };
    Ok(Prepared {
        env,
        setup: ChildSetup {
            cgroup_procs,
            limits,
            ids,
            cwd,
        },
    })
}

/// The effective uid of this process.
pub fn euid() -> u32 {
    unsafe { libc::geteuid() }
}

/// MemTotal of /proc/meminfo, in KiB.
pub fn parse_mem_total_kib(meminfo: &str) -> Option<u64> {
    meminfo.lines().find_map(|line| {
        let rest = line.strip_prefix("MemTotal:")?;
        rest.split_whitespace().next()?.parse().ok()
    })
}

/// Creates the cgroup every exec joins and sets its limits: at most `max_procs`
/// processes and `memory_percent` percent of the guest's memory (0 leaves a
/// limit off). A fork bomb or a runaway allocation in a command then hits the
/// cgroup's limit, not the guest's. `root` is the cgroup2 mount.
pub fn setup_cgroup(root: &Path, name: &str, max_procs: u64, memory_percent: u64, mem_total_kib: Option<u64>) -> io::Result<PathBuf> {
    if !root.join("cgroup.controllers").exists() {
        return Err(io::Error::new(
            io::ErrorKind::Unsupported,
            format!("{} is not a cgroup2 mount", root.display()),
        ));
    }
    let dir = root.join(name);
    match std::fs::create_dir(&dir) {
        Ok(()) => {}
        Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {}
        Err(e) => return Err(e),
    }
    // Make the controllers available to the new cgroup. systemd usually has
    // them on already; a failure here shows up when the limit below is written.
    let _ = std::fs::write(root.join("cgroup.subtree_control"), "+pids +memory");
    if max_procs > 0 {
        std::fs::write(dir.join("pids.max"), max_procs.to_string())?;
    }
    if memory_percent > 0 {
        let total = mem_total_kib.ok_or_else(|| io::Error::other("guest memory size unknown"))?;
        let bytes = total * 1024 / 100 * memory_percent.min(100);
        std::fs::write(dir.join("memory.max"), bytes.to_string())?;
    }
    Ok(dir)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    struct Fake {
        names: HashMap<String, Account>,
        uids: HashMap<u32, Account>,
        owners: HashMap<PathBuf, (u32, u32)>,
    }

    fn acct(name: &str, uid: u32, gid: u32) -> Account {
        Account {
            name: name.into(),
            uid,
            gid,
            home: PathBuf::from(format!("/home/{name}")),
        }
    }

    impl Fake {
        fn new() -> Fake {
            let sandboxd = acct("sandboxd", 10001, 10001);
            let dev = acct("dev", 1000, 1000);
            Fake {
                names: HashMap::from([("sandboxd".into(), sandboxd.clone()), ("dev".into(), dev.clone())]),
                uids: HashMap::from([(10001, sandboxd), (1000, dev)]),
                owners: HashMap::new(),
            }
        }
    }

    impl Accounts for Fake {
        fn by_name(&self, name: &str) -> Option<Account> {
            self.names.get(name).cloned()
        }
        fn by_uid(&self, uid: u32) -> Option<Account> {
            self.uids.get(&uid).cloned()
        }
        fn owner_of(&self, path: &Path) -> Option<(u32, u32)> {
            self.owners.get(path).copied()
        }
    }

    fn policy() -> ExecPolicy {
        ExecPolicy {
            default_user: Some("sandboxd".into()),
            workspace_dir: PathBuf::from("/workspace"),
            limits: Limits::NONE,
            cgroup: None,
        }
    }

    #[test]
    fn a_command_is_not_root_by_default() {
        let f = Fake::new();
        // No workspace owner: the default account.
        assert_eq!(resolve(&policy(), &f, 0, false), Ok(RunAs::User(acct("sandboxd", 10001, 10001))));
    }

    #[test]
    fn as_root_keeps_root() {
        assert_eq!(resolve(&policy(), &Fake::new(), 0, true), Ok(RunAs::Inherit));
    }

    #[test]
    fn the_workspace_owner_wins_over_the_default_account() {
        let mut f = Fake::new();
        f.owners.insert(PathBuf::from("/workspace"), (1000, 1001));
        // The directory's gid, not the account's primary group: virtiofs keeps the host's ids.
        match resolve(&policy(), &f, 0, false) {
            Ok(RunAs::User(a)) => assert_eq!((a.name.as_str(), a.uid, a.gid), ("dev", 1000, 1001)),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn an_owner_without_an_account_gets_tmp_as_home() {
        let mut f = Fake::new();
        f.owners.insert(PathBuf::from("/workspace"), (4242, 4242));
        match resolve(&policy(), &f, 0, false) {
            Ok(RunAs::User(a)) => {
                assert_eq!((a.name.as_str(), a.uid, a.gid), ("", 4242, 4242));
                assert_eq!(a.home, PathBuf::from("/tmp"));
            }
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn a_root_owned_workspace_gives_no_owner() {
        let mut f = Fake::new();
        f.owners.insert(PathBuf::from("/workspace"), (0, 0));
        assert_eq!(resolve(&policy(), &f, 0, false), Ok(RunAs::User(acct("sandboxd", 10001, 10001))));
    }

    #[test]
    fn a_missing_default_account_is_an_error_not_root() {
        let mut f = Fake::new();
        f.names.clear();
        match resolve(&policy(), &f, 0, false) {
            Err(ResolveError::Misconfigured(m)) => assert!(m.contains("sandboxd") && m.contains("as_root"), "{m}"),
            other => panic!("{other:?}"),
        }
        // ...and as_root still works.
        assert_eq!(resolve(&policy(), &f, 0, true), Ok(RunAs::Inherit));
    }

    #[test]
    fn a_daemon_that_is_not_root_runs_commands_as_itself_and_refuses_as_root() {
        let f = Fake::new();
        assert_eq!(resolve(&policy(), &f, 1000, false), Ok(RunAs::Inherit));
        match resolve(&policy(), &f, 1000, true) {
            Err(ResolveError::Denied(m)) => assert!(m.contains("uid 1000"), "{m}"),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn switching_off_keeps_the_old_behaviour() {
        let f = Fake::new();
        assert_eq!(resolve(&ExecPolicy::no_switch(), &f, 0, false), Ok(RunAs::Inherit));
        assert_eq!(resolve(&ExecPolicy::no_switch(), &f, 0, true), Ok(RunAs::Inherit));
    }

    #[test]
    fn errors_map_to_io_kinds() {
        let denied: io::Error = ResolveError::Denied("no".into()).into();
        assert_eq!(denied.kind(), io::ErrorKind::PermissionDenied);
        assert!(is_refusal(&denied));
        assert_eq!(denied.to_string(), "no");
        let broken: io::Error = ResolveError::Misconfigured("bad".into()).into();
        assert!(!is_refusal(&broken));
        // The operating system's own EACCES is not a refusal by the policy.
        assert!(!is_refusal(&io::Error::from_raw_os_error(libc::EACCES)));
    }

    #[test]
    fn limits_are_lowered_never_raised() {
        // Ask for more than any hard limit can be: the result is the current hard limit.
        let (_, lim) = lowered(Rl::Nofile, u64::MAX).expect("getrlimit");
        let mut cur = libc::rlimit { rlim_cur: 0, rlim_max: 0 };
        assert_eq!(Rl::Nofile.get(&mut cur), 0);
        assert_eq!(lim.rlim_max, cur.rlim_max);
        assert_eq!(lim.rlim_cur, cur.rlim_max);
        let (_, small) = lowered(Rl::Nofile, 256).expect("getrlimit");
        assert_eq!((small.rlim_cur, small.rlim_max), (256.min(cur.rlim_max), 256.min(cur.rlim_max)));
        let (_, core) = lowered(Rl::Core, 0).expect("getrlimit");
        assert_eq!((core.rlim_cur, core.rlim_max), (0, 0));
    }

    #[test]
    fn prepare_sets_home_user_and_the_limits_asked_for() {
        let f = Fake::new();
        let mut p = policy();
        p.limits = Limits {
            max_procs: 100,
            max_open_files: 128,
            core_dumps: false,
        };
        let prepared = prepare(&p, &f, 0, false, Some("/tmp")).expect("prepare");
        assert_eq!(prepared.ids(), Some((10001, 10001)));
        assert!(prepared.env.contains(&("HOME", "/home/sandboxd".into())));
        assert!(prepared.env.contains(&("USER", "sandboxd".into())));
        assert_eq!(prepared.setup.limits.len(), 3);
        assert!(prepared.setup.cwd.is_some());

        let root = prepare(&p, &f, 0, true, None).expect("prepare as root");
        assert!(root.env.is_empty());
        assert_eq!(root.ids(), None);
        // Limits apply to root's commands too.
        assert_eq!(root.setup.limits.len(), 3);
    }

    #[test]
    fn prepare_rejects_a_cwd_with_a_nul_byte() {
        let err = prepare(&ExecPolicy::no_switch(), &Fake::new(), 0, false, Some("a\0b")).err().expect("error");
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
    }

    #[test]
    fn mem_total_parses() {
        assert_eq!(parse_mem_total_kib("MemTotal:        2036428 kB\nMemFree: 1 kB\n"), Some(2036428));
        assert_eq!(parse_mem_total_kib("MemFree: 1 kB\n"), None);
    }

    #[test]
    fn cgroup_setup_writes_the_limits() {
        let root = std::env::temp_dir().join(format!("pd-cgroup-{}-{}", std::process::id(), line!()));
        std::fs::create_dir_all(&root).unwrap();
        // Not a cgroup2 mount.
        let err = setup_cgroup(&root, "asp-exec", 100, 50, Some(1000)).unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::Unsupported);

        std::fs::write(root.join("cgroup.controllers"), "cpu memory pids\n").unwrap();
        let dir = setup_cgroup(&root, "asp-exec", 4096, 80, Some(1_000_000)).expect("setup");
        assert_eq!(dir, root.join("asp-exec"));
        assert_eq!(std::fs::read_to_string(dir.join("pids.max")).unwrap(), "4096");
        // 1_000_000 KiB = 1_024_000_000 bytes; 80 percent of it.
        assert_eq!(std::fs::read_to_string(dir.join("memory.max")).unwrap(), "819200000");
        assert_eq!(std::fs::read_to_string(root.join("cgroup.subtree_control")).unwrap(), "+pids +memory");

        // A limit of 0 leaves the file alone, and a second setup is fine.
        let again = root.join("second");
        std::fs::create_dir_all(&again).unwrap();
        std::fs::write(again.join("cgroup.controllers"), "").unwrap();
        let d2 = setup_cgroup(&again, "x", 0, 0, None).expect("no limits");
        assert!(!d2.join("pids.max").exists() && !d2.join("memory.max").exists());
        assert!(setup_cgroup(&root, "asp-exec", 10, 0, None).is_ok());
        assert_eq!(std::fs::read_to_string(dir.join("pids.max")).unwrap(), "10");
        // A memory percentage needs the memory size.
        assert!(setup_cgroup(&root, "asp-exec", 0, 50, None).is_err());
        let _ = std::fs::remove_dir_all(&root);
    }
}
