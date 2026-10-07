package reconciler

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func guestNetFor(t *testing.T, slot int) tap.GuestNet {
	t.Helper()
	g, err := tap.Slot(netip.MustParsePrefix("10.200.0.0/16"), slot)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A guest learns who it is from its kernel command line: its name, its resolver
// when something answers there, and the proxy variables when the node has a proxy.
func TestGuestNetArgs(t *testing.T) {
	const id = "4252d313-6afb-48da-99d3-0f432b1e2fbc"
	g := guestNetFor(t, 0)

	bare := guestNetArgs(id, g, false, 0)
	if bare != "ip=10.200.0.2::10.200.0.1:255.255.255.252:asp-4252d313:eth0:off" {
		t.Fatalf("a node with no sink and no proxy: %q", bare)
	}

	full := guestNetArgs(id, g, true, 8888)
	for _, want := range []string{
		"ip=10.200.0.2::10.200.0.1:255.255.255.252:asp-4252d313:eth0:off:10.200.0.1",
		"systemd.setenv=HTTP_PROXY=http://10.200.0.1:8888",
		"systemd.setenv=HTTPS_PROXY=http://10.200.0.1:8888",
		"systemd.setenv=http_proxy=http://10.200.0.1:8888",
		"systemd.setenv=https_proxy=http://10.200.0.1:8888",
		"systemd.setenv=NO_PROXY=localhost,127.0.0.1,::1",
		"systemd.setenv=no_proxy=localhost,127.0.0.1,::1",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("args lack %q:\n%s", want, full)
		}
	}
	// One token per setting and no whitespace inside a value: the command line is
	// split on spaces.
	for _, tok := range strings.Fields(full) {
		if !strings.HasPrefix(tok, "ip=") && !strings.HasPrefix(tok, "systemd.setenv=") {
			t.Errorf("unexpected token %q", tok)
		}
	}
	if len(full) > 700 {
		t.Errorf("the guest arguments take %d bytes of a 2048-byte command line", len(full))
	}

	// Another sandbox has another name and gateway.
	other := guestNetArgs("bbbbbbbb-0000-4000-8000-000000000000", guestNetFor(t, 1), true, 3128)
	if !strings.Contains(other, "asp-bbbbbbbb") || !strings.Contains(other, "10.200.0.5") || !strings.Contains(other, "http://10.200.0.5:3128") {
		t.Errorf("second guest: %s", other)
	}

	// A proxy but no resolver: the guest gets the proxy and no nameserver.
	proxyOnly := guestNetArgs(id, g, false, 8888)
	if strings.Contains(strings.Fields(proxyOnly)[0], "eth0:off:") || !strings.Contains(proxyOnly, "HTTP_PROXY") {
		t.Errorf("proxy without resolver: %s", proxyOnly)
	}
}

// The VM a node starts carries its guest's network identity on the command line,
// after the base arguments and the same for the resolver and the proxy the node
// was told about.
func TestStartPutsTheGuestNetworkIdentityOnTheCommandLine(t *testing.T) {
	cp := newFakeCP(t, "cccccccc-0003-4000-8000-000000000003")
	fake := vmm.NewFakeVMM(nil)
	rec := New(cp.client(), "n1", fake, nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.TapAuto = true
	rec.Tap = &tap.Manager{Runner: &tap.RecordingRunner{}}
	rec.Egress = &egress.PolicyCache{}
	rec.GuestDNS = true
	rec.GuestProxyPort = 8888
	rec.tick(context.Background())

	if len(fake.Configs) != 1 {
		t.Fatalf("started %d VMs", len(fake.Configs))
	}
	cmdline := fake.Configs[0].Cmdline
	for _, want := range []string{
		"console=ttyS0 root=/dev/vda",
		"ip=10.200.0.2::10.200.0.1:255.255.255.252:asp-cccccccc:eth0:off:10.200.0.1",
		"systemd.setenv=HTTP_PROXY=http://10.200.0.1:8888",
		"systemd.setenv=NO_PROXY=localhost,127.0.0.1,::1",
	} {
		if !strings.Contains(cmdline, want) {
			t.Errorf("cmdline lacks %q:\n%s", want, cmdline)
		}
	}
}
