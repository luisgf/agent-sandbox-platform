package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"time"
)

// DNSSink is a tiny UDP DNS server that answers A queries for allowlisted
// hostnames (via system LookupIP) and NXDOMAIN for everything else.
// Intended as optional --egress-dns-sink (e.g. :5353). Guests that must not
// bypass HTTP proxy should also block UDP/53 toward the upstream resolver
// (nftables) and point resolv.conf at this sink or the host TAP IP.
type DNSSink struct {
	Allowlist *Allowlist
	// Cache maps the query's source address to its sandbox's allowlist.
	Cache  *PolicyCache
	Logger *slog.Logger
	// Enforce: when false, still NXDOMAIN non-allowlisted (sink is always deny-default).
	Enforce bool
	// Lookup resolves an allowed name; nil uses net.DefaultResolver.LookupIP
	// (tests inject a fake).
	Lookup func(ctx context.Context, network, host string) ([]net.IP, error)
}

// DNS header bits and response codes (RFC 1035 §4.1.1).
const (
	dnsFlagQR     = 0x8000
	dnsFlagRD     = 0x0100
	dnsFlagRA     = 0x0080
	dnsOpcodeMask = 0x7800

	dnsRcodeNoError  = 0
	dnsRcodeServFail = 2
	dnsRcodeNXDomain = 3
	dnsRcodeNotImp   = 4

	dnsTypeA    = 1
	dnsTypeAAAA = 28
)

// allowlist picks the policy of the sandbox that sent the query. Sources that
// are not a known sandbox get the node-wide Allowlist.
func (d *DNSSink) allowlist(from netip.Addr) *Allowlist {
	if al, known := d.Cache.ForAddr(from); known {
		return al
	}
	if d.Allowlist != nil {
		return d.Allowlist
	}
	return NewAllowlistFromPolicy("deny-default", nil)
}

func (d *DNSSink) lookup(ctx context.Context, name string) ([]net.IP, error) {
	if d.Lookup != nil {
		return d.Lookup(ctx, "ip", name)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", name)
}

// ListenAndServe binds UDP addr and serves until ctx cancel.
func (d *DNSSink) ListenAndServe(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	buf := make([]byte, 1232)
	for {
		n, remote, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				if d.Logger != nil {
					d.Logger.Warn("dns sink read", "error", err)
				}
				return err
			}
		}
		resp := d.answer(buf[:n], addrOf(remote))
		if len(resp) == 0 {
			continue
		}
		_, _ = pc.WriteTo(resp, remote)
	}
}

// answer builds the reply to one query. A name the sandbox may not reach is
// NXDOMAIN. An allowed name always exists as far as the guest is concerned: a
// query type the sink does not serve (MX, TXT, HTTPS…) or a family the name
// has no address in is NODATA (NOERROR, no answers), because NXDOMAIN would
// tell the guest's resolver that the name has no records of any type (RFC
// 2308) and poison its negative cache for the A lookup next to it.
func (d *DNSSink) answer(req []byte, from netip.Addr) []byte {
	if len(req) < 12 {
		return nil
	}
	name, qtype, _, ok := parseDNSQuestion(req)
	if !ok {
		return nil
	}
	id := binary.BigEndian.Uint16(req[0:2])
	reqFlags := binary.BigEndian.Uint16(req[2:4])
	reply := func(rcode int, addrs []net.IP) []byte {
		return buildDNSResponse(id, reqFlags, name, qtype, addrs, rcode)
	}
	if reqFlags&dnsOpcodeMask != 0 {
		return reply(dnsRcodeNotImp, nil)
	}
	if d.allowlist(from).Check(name) != nil {
		return reply(dnsRcodeNXDomain, nil)
	}
	if qtype != dnsTypeA && qtype != dnsTypeAAAA {
		return reply(dnsRcodeNoError, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := d.lookup(ctx, name)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return reply(dnsRcodeNXDomain, nil)
		}
		return reply(dnsRcodeServFail, nil)
	}
	var addrs []net.IP
	for _, ip := range ips {
		if qtype == dnsTypeA && ip.To4() != nil {
			addrs = append(addrs, ip.To4())
		}
		if qtype == dnsTypeAAAA && ip.To4() == nil && ip.To16() != nil {
			addrs = append(addrs, ip.To16())
		}
	}
	return reply(dnsRcodeNoError, addrs)
}

func parseDNSQuestion(msg []byte) (name string, qtype uint16, end int, ok bool) {
	if len(msg) < 12 {
		return "", 0, 0, false
	}
	i := 12
	var labels []string
	for i < len(msg) {
		l := int(msg[i])
		i++
		if l == 0 {
			break
		}
		if l&0xC0 == 0xC0 { // compression — not expected in question of simple queries
			return "", 0, 0, false
		}
		if i+l > len(msg) {
			return "", 0, 0, false
		}
		labels = append(labels, string(msg[i:i+l]))
		i += l
	}
	if i+4 > len(msg) {
		return "", 0, 0, false
	}
	qtype = binary.BigEndian.Uint16(msg[i : i+2])
	end = i + 4 // type + class
	return strings.ToLower(strings.Join(labels, ".")), qtype, end, true
}

// buildDNSResponse answers the question with addrs (only when rcode is
// NOERROR). RD is echoed from the query; RA is set because the sink resolves
// recursively on the guest's behalf.
func buildDNSResponse(id, reqFlags uint16, name string, qtype uint16, addrs []net.IP, rcode int) []byte {
	if rcode != dnsRcodeNoError {
		addrs = nil
	}
	out := make([]byte, 0, 512)
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[0:2], id)
	flags := uint16(dnsFlagQR|dnsFlagRA) | reqFlags&dnsFlagRD | uint16(rcode&0x0f)
	binary.BigEndian.PutUint16(hdr[2:4], flags)
	binary.BigEndian.PutUint16(hdr[4:6], 1) // QDCOUNT
	binary.BigEndian.PutUint16(hdr[6:8], uint16(len(addrs)))
	out = append(out, hdr[:]...)

	// Question
	out = appendDNSName(out, name)
	var qc [4]byte
	binary.BigEndian.PutUint16(qc[0:2], qtype)
	binary.BigEndian.PutUint16(qc[2:4], 1) // IN
	out = append(out, qc[:]...)

	for _, ip := range addrs {
		out = appendDNSName(out, name)
		var rr [10]byte
		binary.BigEndian.PutUint16(rr[0:2], qtype)
		binary.BigEndian.PutUint16(rr[2:4], 1)  // IN
		binary.BigEndian.PutUint32(rr[4:8], 30) // TTL
		binary.BigEndian.PutUint16(rr[8:10], uint16(len(ip)))
		out = append(out, rr[:]...)
		out = append(out, ip...)
	}
	return out
}

func appendDNSName(out []byte, name string) []byte {
	if name == "" || name == "." {
		return append(out, 0)
	}
	for _, part := range strings.Split(name, ".") {
		if part == "" {
			continue
		}
		out = append(out, byte(len(part)))
		out = append(out, part...)
	}
	return append(out, 0)
}
