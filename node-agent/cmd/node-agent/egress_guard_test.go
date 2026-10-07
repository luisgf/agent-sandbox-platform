package main

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
)

func TestEgressGuardFromConfig(t *testing.T) {
	g, err := egressGuard(config{EgressAllowCIDRs: "10.50.0.0/16,10.200.1.0/24", GuestSubnet: "10.200.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Check(netip.MustParseAddr("10.50.2.3")); err != nil {
		t.Errorf("an allowed private network is refused: %v", err)
	}
	// The guests' network wins over the allow list: one sandbox must not reach
	// another through the proxy.
	if err := g.Check(netip.MustParseAddr("10.200.1.9")); !errors.Is(err, egress.ErrDestinationBlocked) {
		t.Errorf("the guests' network is reachable: %v", err)
	}
	if err := g.Check(netip.MustParseAddr("10.9.9.9")); err == nil {
		t.Error("a private network nobody allowed is reachable")
	}

	for _, bad := range []config{
		{EgressAllowCIDRs: "10.0.0.0/99"},
		{EgressAllowCIDRs: "not-a-network"},
		{GuestSubnet: "10.200.0.0/16/1"},
	} {
		if _, err := egressGuard(bad); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%+v: error %v does not name the flag", bad, err)
		}
	}
}
