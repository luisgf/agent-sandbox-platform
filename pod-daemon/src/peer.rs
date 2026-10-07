//! Who may talk to the daemon, and where it listens.
//!
//! Whatever reaches the listener can run commands in the guest, as root when it
//! asks. So the listener answers the host and nobody else: a process inside the
//! guest that dials the daemon must not become root by doing so.

use std::ffi::CStr;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};

/// VMADDR_CID_HOST: the hypervisor's side of a vsock connection.
pub const HOST_CID: u32 = 2;

/// A vsock connection is accepted when it comes from the host's CID. A process
/// inside the guest that dials the daemon over vsock's loopback transport comes
/// from CID 1 (VMADDR_CID_LOCAL), and another VM cannot reach the listener.
pub fn vsock_peer_allowed(peer_cid: u32, host_cid: u32) -> bool {
    peer_cid == host_cid
}

/// A TCP connection is accepted from the one address the daemon was told to
/// expect: the host's end of the TAP, which is the guest's default gateway. A
/// process inside the guest that connects to the guest's own address comes from
/// that address instead. With no address to expect, every peer is accepted.
pub fn tcp_peer_allowed(peer: IpAddr, expected: Option<IpAddr>) -> bool {
    match expected {
        Some(ip) => ip == peer,
        None => true,
    }
}

/// Where a TCP daemon listens and whom it answers.
#[derive(Debug, PartialEq, Eq)]
pub struct TcpBind {
    pub addr: SocketAddr,
    pub peer: Option<IpAddr>,
}

pub const DEFAULT_TCP_PORT: u16 = 26500;

/// The default route of /proc/net/route: the interface and the gateway.
pub fn parse_default_route(proc_net_route: &str) -> Option<(String, Ipv4Addr)> {
    for line in proc_net_route.lines().skip(1) {
        let f: Vec<&str> = line.split_whitespace().collect();
        if f.len() < 4 || f[1] != "00000000" {
            continue;
        }
        let flags = u32::from_str_radix(f[3], 16).ok()?;
        if flags & 0x2 == 0 {
            continue; // RTF_GATEWAY
        }
        let gw = u32::from_str_radix(f[2], 16).ok()?;
        // The kernel prints the address as a little-endian hex number.
        return Some((f[0].to_string(), Ipv4Addr::from(gw.to_le_bytes())));
    }
    None
}

/// The first IPv4 address of an interface.
pub fn iface_ipv4(name: &str) -> Option<Ipv4Addr> {
    let mut head: *mut libc::ifaddrs = std::ptr::null_mut();
    if unsafe { libc::getifaddrs(&mut head) } != 0 {
        return None;
    }
    let mut found = None;
    let mut cur = head;
    while !cur.is_null() {
        let ifa = unsafe { &*cur };
        if !ifa.ifa_addr.is_null()
            && unsafe { (*ifa.ifa_addr).sa_family } as i32 == libc::AF_INET
            && unsafe { CStr::from_ptr(ifa.ifa_name) }.to_string_lossy() == name
        {
            let sin = unsafe { &*(ifa.ifa_addr as *const libc::sockaddr_in) };
            found = Some(Ipv4Addr::from(u32::from_be(sin.sin_addr.s_addr)));
            break;
        }
        cur = ifa.ifa_next;
    }
    unsafe { libc::freeifaddrs(head) };
    found
}

/// Works out the TCP listener from the flags.
///
/// `addr` is `auto`, `auto:PORT` (the address of the interface that holds the
/// default route, which is the TAP) or `IP:PORT`. A wildcard address would offer
/// exec on every interface and is refused unless `any_interface` says so. The
/// peer to expect is `peer` when given, else the default gateway when there is
/// one.
pub fn resolve_tcp(
    addr: &str,
    peer: Option<&str>,
    any_interface: bool,
    route: Option<(String, Ipv4Addr)>,
    iface_ip: impl Fn(&str) -> Option<Ipv4Addr>,
) -> Result<TcpBind, String> {
    let gateway = route.as_ref().map(|(_, gw)| IpAddr::V4(*gw));
    let expected = match peer {
        Some(p) => Some(
            p.parse::<IpAddr>()
                .map_err(|_| format!("--tcp-peer {p:?} is not an IP address"))?,
        ),
        None => gateway,
    };
    let addr = addr.trim();
    if addr == "auto" || addr.starts_with("auto:") {
        let port = match addr.strip_prefix("auto:") {
            Some(p) => p.parse::<u16>().map_err(|_| format!("--tcp-addr {addr:?}: bad port"))?,
            None => DEFAULT_TCP_PORT,
        };
        let (iface, _) = route.ok_or("--tcp-addr auto: no default route, so no TAP interface to bind to; give --tcp-addr IP:PORT")?;
        let ip = iface_ip(&iface).ok_or_else(|| format!("--tcp-addr auto: interface {iface} has no IPv4 address"))?;
        return Ok(TcpBind {
            addr: SocketAddr::new(IpAddr::V4(ip), port),
            peer: expected,
        });
    }
    let sock: SocketAddr = addr
        .parse()
        .map_err(|_| format!("--tcp-addr {addr:?} is not auto, auto:PORT or IP:PORT"))?;
    if sock.ip().is_unspecified() && !any_interface {
        return Err(format!(
            "--tcp-addr {addr} listens on every interface, and anything that reaches the daemon can run commands. \
             Bind the guest's address on the TAP (or use auto); --tcp-any-interface allows a wildcard in a lab"
        ));
    }
    Ok(TcpBind { addr: sock, peer: expected })
}

#[cfg(test)]
mod tests {
    use super::*;

    const ROUTE: &str = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n\
eth0\t00000000\t0100C80A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n\
eth0\t0000C80A\t00000000\t0001\t0\t0\t0\tFCFFFFFF\t0\t0\t0\n";

    #[test]
    fn vsock_only_answers_the_host() {
        assert!(vsock_peer_allowed(2, 2));
        assert!(!vsock_peer_allowed(1, 2)); // the guest dialing itself
        assert!(!vsock_peer_allowed(3, 2)); // another guest
        assert!(vsock_peer_allowed(7, 7)); // an unusual host CID, set on purpose
    }

    #[test]
    fn tcp_only_answers_the_expected_peer() {
        let host: IpAddr = "10.200.0.1".parse().unwrap();
        let guest: IpAddr = "10.200.0.2".parse().unwrap();
        assert!(tcp_peer_allowed(host, Some(host)));
        assert!(!tcp_peer_allowed(guest, Some(host)));
        assert!(tcp_peer_allowed(guest, None));
    }

    #[test]
    fn the_default_route_is_read_from_proc() {
        let (iface, gw) = parse_default_route(ROUTE).expect("default route");
        assert_eq!((iface.as_str(), gw), ("eth0", Ipv4Addr::new(10, 200, 0, 1)));
        assert_eq!(parse_default_route("Iface\tDestination\tGateway\tFlags\n"), None);
        // A route to a network without a gateway is not a default route.
        let no_gw = "Iface\tDestination\tGateway\tFlags\neth0\t00000000\t00000000\t0001\n";
        assert_eq!(parse_default_route(no_gw), None);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn an_interface_address_is_found() {
        assert_eq!(iface_ipv4("lo"), Some(Ipv4Addr::LOCALHOST));
        assert_eq!(iface_ipv4("no-such-interface"), None);
    }

    fn route() -> Option<(String, Ipv4Addr)> {
        parse_default_route(ROUTE)
    }

    fn ips(name: &str) -> Option<Ipv4Addr> {
        (name == "eth0").then(|| Ipv4Addr::new(10, 200, 0, 2))
    }

    #[test]
    fn auto_binds_the_tap_address_and_expects_the_gateway() {
        let b = resolve_tcp("auto", None, false, route(), ips).unwrap();
        assert_eq!(b.addr, "10.200.0.2:26500".parse().unwrap());
        assert_eq!(b.peer, Some("10.200.0.1".parse().unwrap()));
        let b = resolve_tcp("auto:9000", None, false, route(), ips).unwrap();
        assert_eq!(b.addr.port(), 9000);
        assert!(resolve_tcp("auto:x", None, false, route(), ips).is_err());
        assert!(resolve_tcp("auto", None, false, None, ips).unwrap_err().contains("no default route"));
        assert!(resolve_tcp("auto", None, false, route(), |_| None).unwrap_err().contains("no IPv4"));
    }

    #[test]
    fn a_wildcard_is_refused_unless_asked_for() {
        for addr in ["0.0.0.0:26500", "[::]:26500"] {
            let err = resolve_tcp(addr, None, false, route(), ips).unwrap_err();
            assert!(err.contains("every interface"), "{err}");
            let ok = resolve_tcp(addr, None, true, route(), ips).unwrap();
            assert!(ok.addr.ip().is_unspecified());
        }
    }

    #[test]
    fn an_explicit_address_and_peer_are_taken_as_given() {
        let b = resolve_tcp("10.200.0.2:26500", Some("10.200.0.99"), false, route(), ips).unwrap();
        assert_eq!(b.addr, "10.200.0.2:26500".parse().unwrap());
        assert_eq!(b.peer, Some("10.200.0.99".parse().unwrap()));
        // No peer flag: the gateway, or nobody when there is no route.
        assert_eq!(resolve_tcp("10.200.0.2:26500", None, false, route(), ips).unwrap().peer, Some("10.200.0.1".parse().unwrap()));
        assert_eq!(resolve_tcp("10.200.0.2:26500", None, false, None, ips).unwrap().peer, None);
        assert!(resolve_tcp("10.200.0.2:26500", Some("gateway"), false, route(), ips).is_err());
        assert!(resolve_tcp("not an address", None, false, route(), ips).is_err());
    }
}
