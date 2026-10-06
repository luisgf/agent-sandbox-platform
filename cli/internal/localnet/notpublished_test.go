package localnet

import (
	"strings"
	"testing"
)

func TestNotPublished(t *testing.T) {
	full := ClientDevice{NodePublic: "pub", Endpoint: "198.51.100.1:50001", Address: "10.188.4.2/30"}
	if msg := full.NotPublished(); msg != "" {
		t.Fatalf("complete grant: %q", msg)
	}
	for _, tc := range []struct {
		d    ClientDevice
		want string
	}{
		{ClientDevice{NodePublic: "pub", Endpoint: "198.51.100.1", Address: "10.188.4.2/30"}, "UDP port"},
		{ClientDevice{Endpoint: "198.51.100.1:50001", Address: "10.188.4.2/30"}, "public key"},
		{ClientDevice{NodePublic: "pub", Endpoint: "198.51.100.1:50001"}, "tunnel address"},
		{ClientDevice{NodePublic: "pub", Address: "10.188.4.2/30"}, "dial address"},
	} {
		if msg := tc.d.NotPublished(); !strings.Contains(msg, tc.want) {
			t.Errorf("%+v: %q does not mention %s", tc.d, msg, tc.want)
		}
	}
}
