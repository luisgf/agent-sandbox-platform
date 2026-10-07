package store

import (
	"context"
	"testing"
)

func TestEffectiveEgressWithRules(t *testing.T) {
	t.Setenv("ASP_EGRESS_DEFAULT_ALLOW", "")
	t.Setenv("ASP_EGRESS_DENY_DEFAULT", "")
	pol := EffectiveEgress("t1", []EgressRule{
		{HostPattern: "*.github.com", Enabled: true},
		{HostPattern: "disabled.example", Enabled: false},
	})
	if pol.Mode != EgressModeDenyDefault || len(pol.Rules) != 1 {
		t.Fatalf("%+v", pol)
	}
}

func TestEffectiveEgressEmptyAllow(t *testing.T) {
	t.Setenv("ASP_EGRESS_DEFAULT_ALLOW", "1")
	t.Setenv("ASP_EGRESS_DENY_DEFAULT", "")
	pol := EffectiveEgress("t1", nil)
	if pol.Mode != EgressModeAllowAll {
		t.Fatalf("%+v", pol)
	}
}

func TestEffectiveEgressEmptyDeny(t *testing.T) {
	t.Setenv("ASP_EGRESS_DEFAULT_ALLOW", "")
	t.Setenv("ASP_EGRESS_DENY_DEFAULT", "1")
	pol := EffectiveEgress("t1", nil)
	if pol.Mode != EgressModeDenyDefault {
		t.Fatalf("%+v", pol)
	}
}

func TestMemoryEgressCRUD(t *testing.T) {
	m := NewMemoryStore()
	port := 443
	out, err := m.PutEgressRules(context.Background(), "t1", []EgressRule{
		{HostPattern: "API.GitHub.com", Port: &port, Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].HostPattern != "api.github.com" {
		t.Fatalf("%+v", out)
	}
	list, err := m.ListEgressRules(context.Background(), "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
}
