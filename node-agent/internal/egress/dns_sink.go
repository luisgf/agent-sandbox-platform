package egress

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
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
	Cache     *PolicyCache
	Logger    *slog.Logger
	// Enforce: when false, still NXDOMAIN non-allowlisted (sink is always deny-default).
	Enforce bool
}

func (d *DNSSink) allowlist() *Allowlist {
	if d.Cache != nil {
		if al := d.Cache.Get(); al != nil {
			return al
		}
	}
	if d.Allowlist != nil {
		return d.Allowlist
	}
	return NewAllowlistFromPolicy("deny-default", nil)
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
		resp := d.answer(buf[:n])
		if len(resp) == 0 {
			continue
		}
		_, _ = pc.WriteTo(resp, remote)
	}
}

func (d *DNSSink) answer(req []byte) []byte {
	if len(req) < 12 {
		return nil
	}
	name, qtype, _, ok := parseDNSQuestion(req)
	if !ok {
		return nil
	}
	// Only A (1) and AAAA (28) — others get NXDOMAIN-ish empty.
	al := d.allowlist()
	allowed := al.Check(name) == nil
	id := binary.BigEndian.Uint16(req[0:2])

	if !allowed || (qtype != 1 && qtype != 28) {
		return buildDNSResponse(id, name, qtype, nil, true)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", name)
	if err != nil || len(ips) == 0 {
		return buildDNSResponse(id, name, qtype, nil, true)
	}
	var addrs []net.IP
	for _, ip := range ips {
		if qtype == 1 && ip.To4() != nil {
			addrs = append(addrs, ip.To4())
		}
		if qtype == 28 && ip.To4() == nil && ip.To16() != nil {
			addrs = append(addrs, ip.To16())
		}
	}
	if len(addrs) == 0 {
		return buildDNSResponse(id, name, qtype, nil, true)
	}
	return buildDNSResponse(id, name, qtype, addrs, false)
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

func buildDNSResponse(id uint16, name string, qtype uint16, addrs []net.IP, nxdomain bool) []byte {
	// Rebuild: ID + flags + counts + question + answers
	out := make([]byte, 0, 512)
	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[0:2], id)
	flags := uint16(0x8000) // QR
	if nxdomain || len(addrs) == 0 {
		flags |= 0x0003 // NXDOMAIN
	} else {
		flags |= 0x0000 // NOERROR
	}
	flags |= 0x0400 // AA bit optional
	binary.BigEndian.PutUint16(hdr[2:4], flags)
	binary.BigEndian.PutUint16(hdr[4:6], 1) // QDCOUNT
	ancount := uint16(len(addrs))
	if nxdomain {
		ancount = 0
	}
	binary.BigEndian.PutUint16(hdr[6:8], ancount)
	out = append(out, hdr[:]...)

	// Question
	out = appendDNSName(out, name)
	var qc [4]byte
	binary.BigEndian.PutUint16(qc[0:2], qtype)
	binary.BigEndian.PutUint16(qc[2:4], 1) // IN
	out = append(out, qc[:]...)

	if nxdomain {
		return out
	}
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
