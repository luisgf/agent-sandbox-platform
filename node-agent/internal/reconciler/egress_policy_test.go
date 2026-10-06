package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/vmm"
)

func policy(version string, hosts ...string) cpclient.EgressPolicy {
	p := cpclient.EgressPolicy{TenantID: "t1", Mode: "deny-default", Version: version}
	for _, h := range hosts {
		p.Rules = append(p.Rules, cpclient.EgressRule{HostPattern: h, Enabled: true})
	}
	return p
}

// A sandbox gets its tenant's policy from the work poll, before any exec, and
// a change on the control plane reaches it on the next poll.
func TestEgressPolicyArrivesWithTheWorkPoll(t *testing.T) {
	cp := newFakeCP(t, "s1")
	cp.egress = map[string]cpclient.EgressPolicy{"t1": policy("v1", "api.github.com")}
	rec := New(cp.client(), "n1", vmm.NewFakeVMM(nil), nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.Egress = &egress.PolicyCache{}

	rec.tick(context.Background())
	al := rec.Egress.Get("s1")
	if al == nil {
		t.Fatal("no policy after the first poll for a sandbox that never executed")
	}
	if al.Check("api.github.com") != nil || al.Check("evil.example") == nil {
		t.Fatal("first policy not applied")
	}

	cp.mu.Lock()
	cp.egress["t1"] = policy("v2", "evil.example")
	cp.mu.Unlock()
	rec.tick(context.Background())
	al = rec.Egress.Get("s1")
	if al.Check("evil.example") != nil || al.Check("api.github.com") == nil {
		t.Fatal("the changed policy did not reach the sandbox on the next poll")
	}

	// A dropped policy for a sandbox still assigned comes back even with the
	// same version.
	rec.Egress.Forget("s1")
	rec.tick(context.Background())
	if rec.Egress.Get("s1") == nil {
		t.Fatal("policy not reapplied after it was dropped")
	}
}

// A control plane that sends no policies leaves the cache alone.
func TestWorkWithoutEgressKeepsPolicies(t *testing.T) {
	cp := newFakeCP(t, "s1")
	rec := New(cp.client(), "n1", vmm.NewFakeVMM(nil), nil, time.Hour)
	rec.VsockDir = t.TempDir()
	rec.Egress = &egress.PolicyCache{}
	set := egress.NewAllowlist("from-exec.example")
	rec.Egress.Set("s1", set)
	rec.tick(context.Background())
	if rec.Egress.Get("s1") != set {
		t.Fatal("a poll without policies replaced the one from exec")
	}
}
