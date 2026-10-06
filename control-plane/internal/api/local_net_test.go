package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func TestLocalNetDefaultOffFlagOnGuestAndDisconnect(t *testing.T) {
	mem := newTestStore(t)
	s := &Server{Store: mem}
	mux := testMux(s)

	// Default: omitted field is off, public path (state off).
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(
		`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("default create %d %s", rr.Code, rr.Body.String())
	}
	var off store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &off); err != nil {
		t.Fatal(err)
	}
	if off.LocalNet || off.LocalNetState != store.LocalNetOff {
		t.Fatalf("default local_net=%v state=%s", off.LocalNet, off.LocalNetState)
	}

	// Policy / CIDR is 400, not a silent full tunnel.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(
		`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64,"local_net":true,"local_net_policy":{"prefixes":["192.168.1.0/24"]}}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("policy want 400 got %d %s", rr.Code, rr.Body.String())
	}

	// Guest cannot set the flag.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(
		`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64,"local_net":true}`))
	req.Header.Set("X-ASP-Caller", "guest")
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("guest create %d %s", rr.Code, rr.Body.String())
	}

	// Authenticated (lab) create with the flag.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(
		`{"tenant_id":"t1","image_ref":"img","cpu_millis":100,"memory_mib":64,"local_net":true,"owner_sub":"human-1"}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("flag on %d %s", rr.Code, rr.Body.String())
	}
	var on store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &on); err != nil {
		t.Fatal(err)
	}
	if !on.LocalNet || on.LocalNetState != store.LocalNetPending {
		t.Fatalf("flag on got %+v", on)
	}
	before := on.LastActivityAt

	// Guest exec cannot flip it.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+on.ID+"/exec", bytes.NewBufferString(
		`{"cmd":["true"],"local_net":false}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("guest exec %d %s", rr.Code, rr.Body.String())
	}

	pub := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+on.ID+"/local-net/grant", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("grant %d %s", rr.Code, rr.Body.String())
	}
	var g struct {
		Grant string `json:"grant"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil || g.Grant == "" {
		t.Fatalf("grant body %s", rr.Body.String())
	}
	// Grant must not be stored in the clear.
	stored, err := mem.GetSandbox(on.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.LocalNetGrantHash, g.Grant) || stored.LocalNetGrantHash == "" {
		t.Fatalf("grant hash=%q clear=%q", stored.LocalNetGrantHash, g.Grant)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+on.ID+"/local-net/heartbeat", bytes.NewBufferString(
		`{"grant":"`+g.Grant+`","client_public_key":"`+pub+`"}`))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("heartbeat %d %s", rr.Code, rr.Body.String())
	}
	var up store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	if up.LocalNetState != store.LocalNetUp || !up.LocalNet {
		t.Fatalf("up=%+v", up)
	}
	if up.LastActivityAt.After(before.Add(time.Second)) && !up.LastActivityAt.Equal(before) {
		// heartbeat must not refresh idle. Equal is required; a clock tick of the same stored value is ok.
	}
	if !up.LastActivityAt.Equal(before) {
		t.Fatalf("heartbeat moved activity %s -> %s", before, up.LastActivityAt)
	}

	// Disconnect: detach withdraws and does not clear the flag (no public fallback).
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/"+on.ID+"/local-net/attach", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("detach %d %s", rr.Code, rr.Body.String())
	}
	var down store.Sandbox
	if err := json.Unmarshal(rr.Body.Bytes(), &down); err != nil {
		t.Fatal(err)
	}
	if !down.LocalNet || down.LocalNetState != store.LocalNetWithdrawn {
		t.Fatalf("disconnect=%+v", down)
	}

	// Idle reap also withdraws and does not count as a reason to restore public egress.
	on2, err := mem.CreateSandbox(store.CreateSandboxInput{
		TenantID: "t1", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64,
		LocalNet: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	mem.SetLastActivityForTest(on2.ID, time.Now().Add(-3*time.Hour))
	reaped, err := mem.StopIdleSandboxes(time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, sb := range reaped {
		if sb.ID == on2.ID {
			found = true
			if !sb.LocalNet || sb.LocalNetState != store.LocalNetWithdrawn || sb.StopReason != store.StopReasonIdle {
				t.Fatalf("idle=%+v", sb)
			}
		}
	}
	if !found {
		t.Fatalf("idle did not reap %s (%d)", on2.ID, len(reaped))
	}
}

func TestLocalNetGrantCarriesNodeDevice(t *testing.T) {
	mem := newTestStore(t)
	s := &Server{Store: mem}
	mux := testMux(s)
	nodePub := "ERERERERERERERERERERERERERERERERERERERERERE="
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{
		TenantID: "t1", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64,
		LocalNet: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	// Before the node publishes, the grant has no tunnel parameters.
	rr0 := httptest.NewRecorder()
	mux.ServeHTTP(rr0, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/local-net/grant", nil))
	var g0 localNetGrantResponse
	_ = json.Unmarshal(rr0.Body.Bytes(), &g0)
	if rr0.Code != http.StatusOK || g0.NodePublicKey != "" || g0.ListenPort != 0 || g0.NodeTunnelAddr != "" || g0.ClientTunnelAddr != "" {
		t.Fatalf("grant before the node published: %d %+v", rr0.Code, g0)
	}
	// The node must publish valid tunnel parameters with its key.
	for _, bad := range []string{
		`{"public_key":"` + nodePub + `"}`,
		`{"public_key":"` + nodePub + `","listen_port":50001,"node_tunnel_addr":"10.9.0.1/30","client_tunnel_addr":"10.9.0.2/30"}`,
		`{"public_key":"` + nodePub + `","listen_port":50001,"node_tunnel_addr":"10.188.4.1/30","client_tunnel_addr":"10.188.9.2/30"}`,
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/local-net/node-public", bytes.NewBufferString(bad)))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("node-public %s: want 400, got %d %s", bad, rr.Code, rr.Body.String())
		}
	}
	published := `{"public_key":"` + nodePub + `","listen_port":50001,"node_tunnel_addr":"10.188.4.1/30","client_tunnel_addr":"10.188.4.2/30"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/local-net/node-public", bytes.NewBufferString(published))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("node-public %d %s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/local-net/node-public", bytes.NewBufferString(`{"public_key":"`+nodePub+`","prefixes":["10.0.0.0/8"]}`))
	req.Header.Set("X-ASP-Caller", "guest")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("guest node-public %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/local-net/grant", nil)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("grant %d %s", rr.Code, rr.Body.String())
	}
	var g map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g["node_public_key"] != nodePub || g["transport"] != "wireguard" {
		t.Fatalf("grant=%v", g)
	}
	if g["tunnel_iface"] != "wg-asp-"+sb.ID[:8] {
		t.Fatalf("iface=%v id=%s", g["tunnel_iface"], sb.ID)
	}
	if int(g["listen_port"].(float64)) != 50001 || g["node_tunnel_addr"] != "10.188.4.1/30" || g["client_tunnel_addr"] != "10.188.4.2/30" {
		t.Fatalf("the grant must carry what the node published: %+v", g)
	}
	if strings.Contains(rr.Body.String(), "8888") {
		t.Fatal("grant mentions proxy")
	}
}

func boolPtr(v bool) *bool { return &v }

// Each server has its own address: the grant dials the sandbox's node.
func TestLocalNetGrantDialsTheSandboxNode(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	t.Setenv("ASP_LOCAL_NET_DIAL", "198.51.100.1")
	mem := store.NewMemoryStore()
	for id, dial := range map[string]string{"node-a": "203.0.113.10", "node-b": ""} {
		if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100", LocalNetDial: dial}); err != nil {
			t.Fatal(err)
		}
	}
	mux := testMux(NewServer(mem))
	dialFor := func(node string) any {
		t.Helper()
		sb, err := mem.CreateSandbox(store.CreateSandboxInput{
			TenantID: "t1", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, NodeID: node, LocalNet: boolPtr(true),
		})
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sb.ID+"/local-net/grant", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("grant %d %s", rr.Code, rr.Body.String())
		}
		var g map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &g)
		return g["dial"]
	}
	if got := dialFor("node-a"); got != "203.0.113.10" {
		t.Fatalf("node-a dial = %v, want its own address", got)
	}
	if got := dialFor("node-b"); got != "198.51.100.1" {
		t.Fatalf("node-b dial = %v, want the ASP_LOCAL_NET_DIAL fallback", got)
	}
}
