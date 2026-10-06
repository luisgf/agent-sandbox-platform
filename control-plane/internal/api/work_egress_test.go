package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func pollWork(t *testing.T, h http.Handler, node string) listWorkResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/nodes/"+node+"/work", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("work: %d %s", rr.Code, rr.Body.String())
	}
	var work listWorkResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &work); err != nil {
		t.Fatal(err)
	}
	return work
}

// The work poll carries each assigned sandbox's egress policy and a version
// that moves when the tenant's rules change what is allowed.
func TestWorkPollCarriesTheEgressPolicy(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	t.Setenv("ASP_EGRESS_DEFAULT_ALLOW", "")
	mem := newTestStore(t, "n1")
	h := testMux(NewServer(mem))
	running, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t1", ImageRef: "img", CPUMillis: 500, MemoryMiB: 256, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	runSandbox(t, mem, running.ID) // never executed, needs no action
	other, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t2", ImageRef: "img", CPUMillis: 500, MemoryMiB: 256, NodeID: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	work := pollWork(t, h, "n1")
	if work.Egress == nil || work.Egress.Tenants[running.ID] != "t1" || work.Egress.Tenants[other.ID] != "t2" {
		t.Fatalf("egress tenants: %+v", work.Egress)
	}
	v1 := work.Egress.Policies["t1"]
	if v1.Mode != store.EgressModeDenyDefault || v1.Version == "" || len(v1.Rules) != 0 {
		t.Fatalf("t1 policy before rules: %+v", v1)
	}

	put := func(body string) {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/v1/tenants/t1/egress", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("put egress: %d %s", rr.Code, rr.Body.String())
		}
	}
	put(`{"rules":[{"host_pattern":"api.github.com"},{"host_pattern":"*.pypi.org","port":443}]}`)
	v2 := pollWork(t, h, "n1").Egress.Policies["t1"]
	if v2.Version == v1.Version || len(v2.Rules) != 2 {
		t.Fatalf("after PUT: version %q (was %q), rules %+v", v2.Version, v1.Version, v2.Rules)
	}
	// The same rules again, in another order: nothing a node must reapply.
	put(`{"rules":[{"host_pattern":"*.pypi.org","port":443},{"host_pattern":"API.github.com"}]}`)
	if v3 := pollWork(t, h, "n1").Egress.Policies["t1"]; v3.Version != v2.Version {
		t.Fatalf("same rules, new version: %q vs %q", v3.Version, v2.Version)
	}
	if t2 := pollWork(t, h, "n1").Egress.Policies["t2"]; t2.Version != v1.Version {
		t.Fatalf("t2 is untouched by t1's rules: %q vs %q", t2.Version, v1.Version)
	}
}

func TestEgressPolicyVersion(t *testing.T) {
	port := 443
	base := store.EgressPolicy{Mode: store.EgressModeDenyDefault, Rules: []store.EgressRule{
		{ID: "a", HostPattern: "api.github.com", Enabled: true},
		{ID: "b", HostPattern: "*.pypi.org", Port: &port, Enabled: true},
	}}
	reordered := store.EgressPolicy{Mode: store.EgressModeDenyDefault, Rules: []store.EgressRule{
		{ID: "x", HostPattern: "*.pypi.org", Port: &port, Enabled: true},
		{ID: "y", HostPattern: "api.github.com", Enabled: true},
		{ID: "z", HostPattern: "evil.example", Enabled: false},
	}}
	if base.Version() != reordered.Version() {
		t.Fatal("ids, order and disabled rules must not change the version")
	}
	allowAll := store.EgressPolicy{Mode: store.EgressModeAllowAll}
	denyAll := store.EgressPolicy{Mode: store.EgressModeDenyDefault}
	if allowAll.Version() == denyAll.Version() || base.Version() == denyAll.Version() {
		t.Fatal("a different decision must change the version")
	}
}
