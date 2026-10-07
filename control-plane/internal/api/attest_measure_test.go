package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/oidc"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

func hexDigest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

type attestEnv struct {
	t    *testing.T
	att  *attest.SoftwareAttestor
	srv  *Server
	mux  http.Handler
	mem  *store.MemoryStore
	sbID string
}

func newAttestEnv(t *testing.T) *attestEnv {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "1")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e := &attestEnv{t: t, att: attest.NewSoftwareAttestorFromKey(key), mem: store.NewMemoryStore()}
	e.srv = NewServer(e.mem)
	e.srv.Attestor = e.att
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.OIDC = oidc.NewSignerFromKey(rsaKey, "http://issuer.test")
	e.mux = testMux(e.srv)
	rr := e.do(http.MethodPost, "/v1/sandboxes", `{"tenant_id":"t1","image_ref":"debian:bookworm","cpu_millis":1000,"memory_mib":512}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	e.sbID = sb.ID
	return e
}

func (e *attestEnv) do(method, path, body string) *httptest.ResponseRecorder {
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(method, path, rdr))
	return rr
}

func (e *attestEnv) statement(kernel, rootfs byte, vmm, boot string) attest.BootStatement {
	return attest.BootStatement{
		SandboxID: e.sbID, ImageDigest: hexDigest(rootfs), KernelDigest: hexDigest(kernel), VMMProfile: "cloud-hypervisor",
		CID: 5, NodeID: "node-a", TS: time.Now().UTC().Format(time.RFC3339), VMMVersion: vmm, Boot: boot,
	}
}

func (e *attestEnv) post(st attest.BootStatement) *httptest.ResponseRecorder {
	ev, err := e.att.Attest(context.Background(), st)
	if err != nil {
		e.t.Fatal(err)
	}
	payload, _ := json.Marshal(ev)
	return e.do(http.MethodPost, "/v1/sandboxes/"+e.sbID+"/attest", string(payload))
}

// claim mints a workload token and returns its x_asp_attestation, or nil.
func (e *attestEnv) claim() map[string]any {
	rr := e.do(http.MethodPost, "/v1/internal/oidc/token", `{"sandbox_id":"`+e.sbID+`","aud":"https://api.example"}`)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("mint %d %s", rr.Code, rr.Body.String())
	}
	var mint map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &mint)
	c, _ := mint["claims"].(map[string]any)["x_asp_attestation"].(map[string]any)
	return c
}

func (e *attestEnv) allowlist(body string) (path string) {
	e.t.Helper()
	path = filepath.Join(e.t.TempDir(), "images.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatal(err)
	}
	al, err := attest.LoadAllowlist(path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.srv.AttestAllow = al
	return path
}

func (e *attestEnv) getAttestation() map[string]any {
	rr := e.do(http.MethodGet, "/v1/sandboxes/"+e.sbID+"/attestation", "")
	if rr.Code != http.StatusOK {
		e.t.Fatalf("get attestation %d %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

// Without an allowlist the digests the node measured are recorded, shown and
// claimed, and the claim says they were not checked against anything.
func TestMeasuredEvidenceIsRecordedAndClaimed(t *testing.T) {
	e := newAttestEnv(t)
	if rr := e.post(e.statement('e', 'a', "cloud-hypervisor v43.0", attest.BootNew)); rr.Code != http.StatusOK {
		t.Fatalf("attest %d %s", rr.Code, rr.Body.String())
	}
	got := e.getAttestation()
	if got["measured"] != true || got["fresh"] != true {
		t.Fatalf("GET: %v", got)
	}
	if _, listed := got["allowlisted"]; listed {
		t.Fatalf("no allowlist is configured, yet the answer says whether the image is on it: %v", got)
	}
	bundle := got["attestation"].(map[string]any)["bundle"].(map[string]any)["statement"].(map[string]any)
	if bundle["kernel_digest"] != hexDigest('e') || bundle["vmm_version"] != "cloud-hypervisor v43.0" || bundle["boot"] != "new" {
		t.Fatalf("the stored statement lost its measurements: %v", bundle)
	}
	if got["attestation"].(map[string]any)["image_digest"] != hexDigest('a') {
		t.Fatalf("record: %v", got["attestation"])
	}

	c := e.claim()
	if c == nil {
		t.Fatal("no claim")
	}
	if c["measured"] != true || c["allowlisted"] != false || c["image_digest"] != hexDigest('a') ||
		c["kernel_digest"] != hexDigest('e') || c["vmm_version"] != "cloud-hypervisor v43.0" || c["boot"] != "new" || c["node_id"] != "node-a" {
		t.Fatalf("claim: %v", c)
	}
	if _, named := c["image_name"]; named {
		t.Fatalf("an unlisted image has a name in the claim: %v", c)
	}
}

// A statement from a node that does not measure still stores (no allowlist),
// but the claim does not put the image reference it carries where a digest goes.
func TestUnmeasuredEvidenceIsClaimedAsSuch(t *testing.T) {
	e := newAttestEnv(t)
	st := attest.BootStatement{SandboxID: e.sbID, ImageDigest: "debian:bookworm", VMMProfile: "cloud-hypervisor", CID: 5, NodeID: "node-a", TS: time.Now().UTC().Format(time.RFC3339)}
	if rr := e.post(st); rr.Code != http.StatusOK {
		t.Fatalf("attest %d %s", rr.Code, rr.Body.String())
	}
	if got := e.getAttestation(); got["measured"] != false {
		t.Fatalf("GET: %v", got)
	}
	c := e.claim()
	if c == nil || c["measured"] != false || c["allowlisted"] != false {
		t.Fatalf("claim: %v", c)
	}
	for _, k := range []string{"image_digest", "kernel_digest", "vmm_version", "boot"} {
		if _, there := c[k]; there {
			t.Errorf("an unmeasured claim carries %s: %v", k, c)
		}
	}
}

// With an allowlist only a known kernel and base image get an attestation.
func TestAllowlistRefusesUnknownImages(t *testing.T) {
	e := newAttestEnv(t)
	e.allowlist(`{"images":[{"name":"asp-debian","kernel":"` + hexDigest('e') + `","rootfs":"` + hexDigest('a') + `","vmm":"cloud-hypervisor v43.0"}]}`)

	for name, st := range map[string]attest.BootStatement{
		"unknown kernel": e.statement('d', 'a', "cloud-hypervisor v43.0", attest.BootNew),
		"unknown image":  e.statement('e', 'd', "cloud-hypervisor v43.0", attest.BootNew),
		"unknown vmm":    e.statement('e', 'a', "cloud-hypervisor v99", attest.BootNew),
		"never measured": {SandboxID: e.sbID, ImageDigest: "debian:bookworm", NodeID: "node-a", TS: time.Now().UTC().Format(time.RFC3339)},
		"only the image": {SandboxID: e.sbID, ImageDigest: hexDigest('a'), NodeID: "node-a", TS: time.Now().UTC().Format(time.RFC3339)},
	} {
		rr := e.post(st)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "attestation refused") {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if rr := e.do(http.MethodGet, "/v1/sandboxes/"+e.sbID+"/attestation", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("a refused statement was stored: %d", rr.Code)
	}
	if c := e.claim(); c != nil {
		t.Fatalf("a claim without evidence: %v", c)
	}
	evs, _ := e.mem.ListEvents(context.Background(), e.sbID)
	refused := 0
	for _, ev := range evs {
		if ev.EventType == "sandbox.attestation_refused" {
			refused++
		}
	}
	if refused != 5 {
		t.Fatalf("%d refusals in the journal, want 5", refused)
	}

	if rr := e.post(e.statement('e', 'a', "cloud-hypervisor v43.0", attest.BootNew)); rr.Code != http.StatusOK {
		t.Fatalf("a listed image was refused: %d %s", rr.Code, rr.Body.String())
	}
	got := e.getAttestation()
	if got["allowlisted"] != true || got["image_name"] != "asp-debian" {
		t.Fatalf("GET: %v", got)
	}
	c := e.claim()
	if c == nil || c["allowlisted"] != true || c["image_name"] != "asp-debian" || c["measured"] != true {
		t.Fatalf("claim: %v", c)
	}
}

// Taking an image off the list withdraws the claim of the sandboxes already
// attested as it.
func TestClaimEndsWhenTheImageLeavesTheAllowlist(t *testing.T) {
	e := newAttestEnv(t)
	path := e.allowlist(`{"images":[{"name":"v1","kernel":"` + hexDigest('e') + `","rootfs":"` + hexDigest('a') + `"},{"name":"v2","kernel":"` + hexDigest('e') + `","rootfs":"` + hexDigest('b') + `"}]}`)
	if rr := e.post(e.statement('e', 'a', "", attest.BootNew)); rr.Code != http.StatusOK {
		t.Fatalf("attest %d %s", rr.Code, rr.Body.String())
	}
	if e.claim() == nil {
		t.Fatal("no claim while the image is listed")
	}

	if err := os.WriteFile(path, []byte(`{"images":[{"name":"v2","kernel":"`+hexDigest('e')+`","rootfs":"`+hexDigest('b')+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, later, later)

	if c := e.claim(); c != nil {
		t.Fatalf("the claim outlived the image's place on the list: %v", c)
	}
	if got := e.getAttestation(); got["allowlisted"] != false {
		t.Fatalf("GET still says the image is allowed: %v", got)
	}
}

func TestStatementWithABadMeasurementIsRefused(t *testing.T) {
	e := newAttestEnv(t)
	st := e.statement('e', 'a', "", attest.BootNew)
	st.KernelDigest = "latest"
	ev, _ := e.att.Attest(context.Background(), e.statement('e', 'a', "", attest.BootNew))
	ev.Statement = st // signed for another statement: refused on validation before the signature even matters
	payload, _ := json.Marshal(ev)
	rr := e.do(http.MethodPost, "/v1/sandboxes/"+e.sbID+"/attest", string(payload))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "kernel_digest") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	st = e.statement('e', 'a', "", "reboot")
	ev.Statement = st
	payload, _ = json.Marshal(ev)
	if rr := e.do(http.MethodPost, "/v1/sandboxes/"+e.sbID+"/attest", string(payload)); rr.Code != http.StatusBadRequest {
		t.Fatalf("an unknown boot kind: %d %s", rr.Code, rr.Body.String())
	}
}
