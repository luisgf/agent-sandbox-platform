package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// dnsQuery builds a query for name and qtype with RD set.
func sinkQuery(id uint16, name string, qtype uint16) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[2:4], dnsFlagRD)
	binary.BigEndian.PutUint16(msg[4:6], 1)
	msg = appendDNSName(msg, name)
	var qc [4]byte
	binary.BigEndian.PutUint16(qc[0:2], qtype)
	binary.BigEndian.PutUint16(qc[2:4], 1)
	return append(msg, qc[:]...)
}

type sinkReply struct {
	id      uint16
	flags   uint16
	rcode   int
	ancount int
	addrs   []net.IP
}

func parseSinkReply(t *testing.T, b []byte) sinkReply {
	t.Helper()
	if len(b) < 12 {
		t.Fatalf("short reply: %d bytes", len(b))
	}
	r := sinkReply{
		id:      binary.BigEndian.Uint16(b[0:2]),
		flags:   binary.BigEndian.Uint16(b[2:4]),
		ancount: int(binary.BigEndian.Uint16(b[6:8])),
	}
	r.rcode = int(r.flags & 0x0f)
	_, _, end, ok := parseDNSQuestion(b)
	if !ok {
		t.Fatal("reply question does not parse")
	}
	i := end
	for n := 0; n < r.ancount; n++ {
		// name (uncompressed labels), type, class, ttl, rdlength, rdata
		for b[i] != 0 {
			i += int(b[i]) + 1
		}
		i++
		rdlen := int(binary.BigEndian.Uint16(b[i+8 : i+10]))
		i += 10
		r.addrs = append(r.addrs, net.IP(b[i:i+rdlen]))
		i += rdlen
	}
	return r
}

func newTestSink(lookup func(context.Context, string, string) ([]net.IP, error)) *DNSSink {
	return &DNSSink{Allowlist: NewAllowlist("v4only.example", "dual.example", "gone.example", "flaky.example"), Lookup: lookup}
}

func fakeSinkLookup(_ context.Context, _ string, host string) ([]net.IP, error) {
	switch host {
	case "v4only.example":
		return []net.IP{net.ParseIP("192.0.2.10")}, nil
	case "dual.example":
		return []net.IP{net.ParseIP("192.0.2.20"), net.ParseIP("2001:db8::20")}, nil
	case "gone.example":
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	default:
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
}

var sinkGuest = netip.MustParseAddr("192.0.2.1")

func TestDNSSinkAnswersAllowedName(t *testing.T) {
	r := parseSinkReply(t, newTestSink(fakeSinkLookup).answer(sinkQuery(7, "dual.example", dnsTypeA), sinkGuest))
	if r.id != 7 || r.rcode != dnsRcodeNoError || r.ancount != 1 || !r.addrs[0].Equal(net.ParseIP("192.0.2.20")) {
		t.Fatalf("A dual.example: %+v", r)
	}
	if r.flags&dnsFlagQR == 0 || r.flags&dnsFlagRA == 0 || r.flags&dnsFlagRD == 0 {
		t.Fatalf("flags %#x: want QR, RA and the query's RD", r.flags)
	}
	r = parseSinkReply(t, newTestSink(fakeSinkLookup).answer(sinkQuery(8, "dual.example", dnsTypeAAAA), sinkGuest))
	if r.rcode != dnsRcodeNoError || r.ancount != 1 || !r.addrs[0].Equal(net.ParseIP("2001:db8::20")) {
		t.Fatalf("AAAA dual.example: %+v", r)
	}
}

// An allowed name with no address in the asked family exists: NODATA, not NXDOMAIN.
func TestDNSSinkNoDataForMissingFamily(t *testing.T) {
	r := parseSinkReply(t, newTestSink(fakeSinkLookup).answer(sinkQuery(1, "v4only.example", dnsTypeAAAA), sinkGuest))
	if r.rcode != dnsRcodeNoError || r.ancount != 0 {
		t.Fatalf("AAAA for a v4-only name: want NOERROR with no answers, got %+v", r)
	}
}

func TestDNSSinkNoDataForUnservedType(t *testing.T) {
	for _, qtype := range []uint16{15, 16, 65} { // MX, TXT, HTTPS
		called := false
		s := newTestSink(func(ctx context.Context, n, h string) ([]net.IP, error) {
			called = true
			return fakeSinkLookup(ctx, n, h)
		})
		r := parseSinkReply(t, s.answer(sinkQuery(2, "dual.example", qtype), sinkGuest))
		if r.rcode != dnsRcodeNoError || r.ancount != 0 {
			t.Fatalf("type %d for an allowed name: want NODATA, got %+v", qtype, r)
		}
		if called {
			t.Fatalf("type %d: the sink must not resolve types it does not serve", qtype)
		}
	}
}

func TestDNSSinkNXDomainForDeniedName(t *testing.T) {
	for _, qtype := range []uint16{dnsTypeA, dnsTypeAAAA, 65} {
		r := parseSinkReply(t, newTestSink(fakeSinkLookup).answer(sinkQuery(3, "evil.example", qtype), sinkGuest))
		if r.rcode != dnsRcodeNXDomain || r.ancount != 0 {
			t.Fatalf("type %d for a denied name: want NXDOMAIN, got %+v", qtype, r)
		}
	}
}

func TestDNSSinkUpstreamErrors(t *testing.T) {
	r := parseSinkReply(t, newTestSink(fakeSinkLookup).answer(sinkQuery(4, "gone.example", dnsTypeA), sinkGuest))
	if r.rcode != dnsRcodeNXDomain {
		t.Fatalf("name the upstream does not know: want NXDOMAIN, got %+v", r)
	}
	r = parseSinkReply(t, newTestSink(fakeSinkLookup).answer(sinkQuery(5, "flaky.example", dnsTypeA), sinkGuest))
	if r.rcode != dnsRcodeServFail {
		t.Fatalf("upstream failure: want SERVFAIL, got %+v", r)
	}
	s := newTestSink(func(context.Context, string, string) ([]net.IP, error) { return nil, errors.New("boom") })
	r = parseSinkReply(t, s.answer(sinkQuery(6, "dual.example", dnsTypeA), sinkGuest))
	if r.rcode != dnsRcodeServFail {
		t.Fatalf("non-DNS error: want SERVFAIL, got %+v", r)
	}
}

func TestDNSSinkRejectsOtherOpcodes(t *testing.T) {
	q := sinkQuery(9, "dual.example", dnsTypeA)
	binary.BigEndian.PutUint16(q[2:4], 2<<11) // opcode STATUS
	r := parseSinkReply(t, newTestSink(fakeSinkLookup).answer(q, sinkGuest))
	if r.rcode != dnsRcodeNotImp {
		t.Fatalf("opcode STATUS: want NOTIMP, got %+v", r)
	}
}
