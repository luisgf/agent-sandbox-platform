package main

import (
	"flag"
	"testing"
)

func TestOptBoolFlag(t *testing.T) {
	var b optBool
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.Var(&b, "r", "")
	if b.set || b.String() != "auto" {
		t.Fatalf("unset: %+v %q", b, b.String())
	}
	if err := fs.Parse([]string{"-r"}); err != nil || !b.set || !b.value {
		t.Fatalf("bare flag: %+v %v", b, err)
	}
	b = optBool{}
	if err := fs.Parse([]string{"-r=false"}); err != nil || !b.set || b.value || b.String() != "false" {
		t.Fatalf("=false: %+v %v", b, err)
	}
	if err := fs.Parse([]string{"-r=maybe"}); err == nil {
		t.Fatal("a non-bool value was accepted")
	}
}

func TestEnvOptBool(t *testing.T) {
	for _, k := range []string{"ASP_T_A", "ASP_T_B"} {
		t.Setenv(k, "")
	}
	if got := envOptBool("ASP_T_A", "ASP_T_B"); got.set {
		t.Fatalf("nothing set: %+v", got)
	}
	for v, want := range map[string]optBool{
		"1": {true, true}, "true": {true, true}, "YES": {true, true}, "on": {true, true},
		"0": {true, false}, "false": {true, false}, "No": {true, false}, "off": {true, false},
		"auto": {}, "2": {},
	} {
		t.Setenv("ASP_T_B", v)
		if got := envOptBool("ASP_T_A", "ASP_T_B"); got != want {
			t.Errorf("ASP_T_B=%q: %+v, want %+v", v, got, want)
		}
	}
	// The first key that is set wins.
	t.Setenv("ASP_T_A", "0")
	t.Setenv("ASP_T_B", "1")
	if got := envOptBool("ASP_T_A", "ASP_T_B"); !got.set || got.value {
		t.Fatalf("first key: %+v", got)
	}
}

// Deny by default has to hold on a node that was given the egress proxy and
// nothing else: the nft redirect is on and its failure refuses the start.
func TestResolveEgressDefaults(t *testing.T) {
	for _, c := range []struct {
		name         string
		cfg          config
		wantRedirect bool
		wantMode     string
		wantSink     string
		wantErr      bool
	}{
		{"proxy on a real node", config{EgressProxyListen: ":8888", NFTDNSAction: "redirect"}, true, "enforce", ":5353", false},
		{"proxy and sink named", config{EgressProxyListen: ":8888", EgressDNSSink: ":9999", NFTDNSAction: "redirect"}, true, "enforce", ":9999", false},
		{"dns dropped, no sink needed", config{EgressProxyListen: ":8888", NFTDNSAction: "drop"}, true, "enforce", "", false},
		{"no proxy, nothing to redirect to", config{NFTDNSAction: "redirect"}, false, "enforce", "", false},
		{"dry-run never redirects by default", config{EgressProxyListen: ":8888", DryRun: true, NFTDNSAction: "redirect"}, false, "soft", "", false},
		{"asked for off", config{EgressProxyListen: ":8888", egressNFTRedirect: optBool{true, false}, NFTDNSAction: "redirect"}, false, "enforce", "", false},
		{"asked for soft", config{EgressProxyListen: ":8888", NFTEgressMode: "SOFT", NFTDNSAction: "redirect"}, true, "soft", ":5353", false},
		{"dry-run asked to redirect", config{EgressProxyListen: ":8888", DryRun: true, egressNFTRedirect: optBool{true, true}, NFTDNSAction: "redirect"}, true, "soft", ":5353", false},
		{"redirect with no proxy", config{egressNFTRedirect: optBool{true, true}, NFTDNSAction: "redirect"}, true, "enforce", "", true},
		{"bad mode", config{NFTEgressMode: "strict"}, false, "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			err := resolveEgress(&cfg)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, want error %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if cfg.EgressNFTRedirect != c.wantRedirect || cfg.NFTEgressMode != c.wantMode || cfg.EgressDNSSink != c.wantSink {
				t.Fatalf("redirect=%v mode=%q sink=%q, want %v %q %q", cfg.EgressNFTRedirect, cfg.NFTEgressMode, cfg.EgressDNSSink, c.wantRedirect, c.wantMode, c.wantSink)
			}
		})
	}
}

// A node reports that it enforces egress only when the proxy listens and the
// rules were really applied in enforce mode: not when soft mode went on without
// them.
func TestEgressEnforced(t *testing.T) {
	for _, c := range []struct {
		name    string
		cfg     config
		applied bool
		want    bool
	}{
		{"all of it", config{EgressProxyListen: ":8888", NFTEgressMode: "enforce"}, true, true},
		{"soft mode applied", config{EgressProxyListen: ":8888", NFTEgressMode: "soft"}, true, false},
		{"rules not applied", config{EgressProxyListen: ":8888", NFTEgressMode: "enforce"}, false, false},
		{"no proxy", config{NFTEgressMode: "enforce"}, true, false},
	} {
		if got := egressEnforced(c.cfg, c.applied); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestListenPort(t *testing.T) {
	for in, want := range map[string]int{
		":8888": 8888, "0.0.0.0:5353": 5353, "127.0.0.1:53": 53, "[::]:9": 9,
		"": 0, "8888": 0, ":": 0, ":x": 0, ":0": 0, ":-1": 0,
	} {
		if got := listenPort(in); got != want {
			t.Errorf("listenPort(%q) = %d, want %d", in, got, want)
		}
	}
}

// What a guest is told about the network follows what its node really offers.
func TestGuestResolverAndProxy(t *testing.T) {
	redirect := config{EgressProxyListen: ":8888", EgressDNSSink: ":5353", EgressNFTRedirect: true, NFTDNSAction: "redirect"}
	if guestProxyPort(redirect) != 8888 || !guestDNS(redirect) {
		t.Fatal("a node with the proxy, the sink and the redirect offers both")
	}
	for name, c := range map[string]config{
		"no sink":                 {EgressProxyListen: ":8888", EgressNFTRedirect: true, NFTDNSAction: "redirect"},
		"sink but no redirect":    {EgressProxyListen: ":8888", EgressDNSSink: ":5353", NFTDNSAction: "redirect"},
		"dns dropped by the nft":  {EgressProxyListen: ":8888", EgressDNSSink: ":5353", EgressNFTRedirect: true, NFTDNSAction: "drop"},
		"a sink nobody can reach": {EgressDNSSink: ":5353", NFTDNSAction: "redirect"},
	} {
		if guestDNS(c) {
			t.Errorf("%s: the guest would be given a resolver that does not answer", name)
		}
	}
	if !guestDNS(config{EgressDNSSink: "0.0.0.0:53"}) {
		t.Error("a sink on port 53 answers without a redirect")
	}
	if guestProxyPort(config{}) != 0 || guestProxyPort(config{EgressProxyListen: "garbage"}) != 0 {
		t.Error("no proxy, no proxy port")
	}
}
