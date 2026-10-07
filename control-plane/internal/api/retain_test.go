package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Stop, resume and delete over HTTP (ADR-0012).

func doJSON(t *testing.T, h http.Handler, method, path string) (int, store.Sandbox, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var sb store.Sandbox
	_ = json.Unmarshal(rr.Body.Bytes(), &sb)
	return rr.Code, sb, rr.Body.String()
}

func newLifecycleFixture(t *testing.T) (*store.MemoryStore, http.Handler) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := newTestStore(t, "n1")
	return mem, testMux(NewServer(mem))
}

func newPlacedSandbox(t *testing.T, mem *store.MemoryStore) string {
	t.Helper()
	sb, err := mem.CreateSandbox(store.CreateSandboxInput{TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 64, OwnerSub: "user:a"})
	if err != nil {
		t.Fatal(err)
	}
	return sb.ID
}

// Stop keeps the sandbox, resume brings it back on its node, delete ends it.
func TestStopResumeDeleteOverHTTP(t *testing.T) {
	mem, h := newLifecycleFixture(t)
	id := newPlacedSandbox(t, mem)
	runSandbox(t, mem, id)

	code, sb, body := doJSON(t, h, http.MethodPost, "/v1/sandboxes/"+id+"/stop")
	if code != http.StatusOK || sb.State != store.SandboxStopping {
		t.Fatalf("stop: %d %s", code, body)
	}
	if code, _, _ := doJSON(t, h, http.MethodPost, "/v1/sandboxes/"+id+"/start"); code != http.StatusConflict {
		t.Fatalf("resume while stopping: want 409, got %d", code)
	}
	if _, err := mem.UpdateSandboxStatus(id, store.SandboxStopped, "vmm stopped, disk kept"); err != nil {
		t.Fatal(err)
	}

	code, sb, body = doJSON(t, h, http.MethodPost, "/v1/sandboxes/"+id+"/start")
	if code != http.StatusOK || sb.State != store.SandboxRequested || sb.BootCount != 2 {
		t.Fatalf("resume: %d %s", code, body)
	}
	runSandbox(t, mem, id)

	code, sb, body = doJSON(t, h, http.MethodDelete, "/v1/sandboxes/"+id)
	if code != http.StatusOK || sb.State != store.SandboxDeleting {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, _, _ := doJSON(t, h, http.MethodPost, "/v1/sandboxes/"+id+"/stop"); code != http.StatusConflict {
		t.Fatalf("stop of a deleting sandbox: want 409, got %d", code)
	}
	for _, p := range []string{"/stop", "/start"} {
		if code, _, _ := doJSON(t, h, http.MethodPost, "/v1/sandboxes/nope"+p); code != http.StatusNotFound {
			t.Fatalf("%s on an unknown sandbox: want 404, got %d", p, code)
		}
	}
}

// A resume that no node can take is refused like a pinned create: 503 when the
// node is full, 409 when it cannot take sandboxes at all. The sandbox stays stopped.
func TestResumeRefusalsUsePlacementStatuses(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	mem := store.NewMemoryStore()
	if _, err := mem.RegisterNode(store.RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100", MaxSandboxes: 1}); err != nil {
		t.Fatal(err)
	}
	h := testMux(NewServer(mem))
	first := newPlacedSandbox(t, mem)
	runSandbox(t, mem, first)
	if _, err := mem.StopSandbox(first, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.UpdateSandboxStatus(first, store.SandboxStopped, ""); err != nil {
		t.Fatal(err)
	}
	second := newPlacedSandbox(t, mem) // takes the only slot while the first is stopped

	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+first+"/start", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("full node: want 503 with Retry-After, got %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := mem.GetSandbox(first); got.State != store.SandboxStopped {
		t.Fatalf("state=%s after a refused resume", got.State)
	}

	if _, err := mem.StopSandbox(second, ""); err != nil { // requested → stopped, frees the slot
		t.Fatal(err)
	}
	if _, err := mem.SetNodeCordoned("n1", true); err != nil {
		t.Fatal(err)
	}
	if code, _, body := doJSON(t, h, http.MethodPost, "/v1/sandboxes/"+first+"/start"); code != http.StatusConflict {
		t.Fatalf("cordoned node: want 409, got %d %s", code, body)
	}
}

// exec on a sandbox that is not running says what to do about it.
func TestExecMessagesNameTheFix(t *testing.T) {
	f := newExecStateFixture(t, okAgent)
	t.Setenv("ASP_AUTO_PROVISION", "0")
	stopped := f.sandbox()
	runSandbox(t, f.mem, stopped)
	if _, err := f.mem.StopSandbox(stopped, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mem.UpdateSandboxStatus(stopped, store.SandboxStopped, ""); err != nil {
		t.Fatal(err)
	}
	deleting := f.sandbox()
	runSandbox(t, f.mem, deleting)
	if _, err := f.mem.DeleteSandbox(deleting, ""); err != nil {
		t.Fatal(err)
	}
	deleted := f.sandbox()
	if _, err := f.mem.DeleteSandbox(deleted, ""); err != nil { // never claimed: deleted at once
		t.Fatal(err)
	}
	failed := f.sandbox()
	runSandbox(t, f.mem, failed)
	if _, err := f.mem.UpdateSandboxStatus(failed, store.SandboxFailed, "tap: denied"); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ id, want string }{
		"stopped":  {stopped, "resume it (asp session resume)"},
		"deleting": {deleting, "being deleted"},
		"deleted":  {deleted, "was deleted"},
		"failed":   {failed, "sandbox failed: tap: denied"},
	} {
		rr := f.post(tc.id, "/exec")
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("%s: want 409 with %q, got %d %s", name, tc.want, rr.Code, rr.Body.String())
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("the node agent got %d calls for sandboxes that are not running", n)
	}
}

// A stopped sandbox that the idle reaper stopped keeps the specific message,
// and says its disk is kept.
func TestExecIdleReapedMessageSaysResume(t *testing.T) {
	sb := store.Sandbox{State: store.SandboxStopped, StopReason: store.StopReasonIdle}
	if msg := execBlock(sb); !strings.Contains(msg, "idle timeout") || !strings.Contains(msg, "asp session resume") {
		t.Fatalf("message=%q", msg)
	}
	lost := store.Sandbox{State: store.SandboxStopped, StopReason: store.StopReasonNodeLost}
	if msg := execBlock(lost); msg != store.NodeLostMessage {
		t.Fatalf("message=%q", msg)
	}
	if msg := execBlock(store.Sandbox{State: store.SandboxStopped, StatusDetail: "vm.boot failed"}); !strings.Contains(msg, "last resume failed: vm.boot failed") {
		t.Fatalf("message=%q", msg)
	}
	// After a resume the old reason no longer applies.
	if msg := execBlock(store.Sandbox{State: store.SandboxStarting, StopReason: ""}); !strings.Contains(msg, "wait until it is running") {
		t.Fatalf("message=%q", msg)
	}
}

// The list hides deleted sandboxes unless asked.
func TestListHidesDeletedSandboxes(t *testing.T) {
	mem, h := newLifecycleFixture(t)
	keep := newPlacedSandbox(t, mem)
	gone := newPlacedSandbox(t, mem)
	if _, err := mem.DeleteSandbox(gone, ""); err != nil {
		t.Fatal(err)
	}
	list := func(q string) []string {
		req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes"+q, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var out listSandboxesResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		var ids []string
		for _, sb := range out.Sandboxes {
			ids = append(ids, sb.ID)
		}
		return ids
	}
	if got := list(""); len(got) != 1 || got[0] != keep {
		t.Fatalf("list=%v, want only %s", got, keep)
	}
	if got := list("?include_deleted=1"); len(got) != 2 {
		t.Fatalf("list with deleted=%v", got)
	}
	if code, sb, _ := doJSON(t, h, http.MethodGet, "/v1/sandboxes/"+gone); code != http.StatusOK || sb.State != store.SandboxDeleted {
		t.Fatalf("a deleted sandbox can still be read: %d %+v", code, sb)
	}
}

// /work always carries the retained list, even empty: that is how a node knows
// stopping keeps disks.
func TestWorkCarriesTheRetainedList(t *testing.T) {
	mem, h := newLifecycleFixture(t)
	id := newPlacedSandbox(t, mem)
	runSandbox(t, mem, id)

	work := func() map[string]json.RawMessage {
		req := httptest.NewRequest(http.MethodGet, "/v1/nodes/n1/work", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("work=%d %s", rr.Code, rr.Body.String())
		}
		var out map[string]json.RawMessage
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return out
	}
	if got := string(work()["retained"]); got != "[]" {
		t.Fatalf("retained=%s, want []", got)
	}
	if _, err := mem.StopSandbox(id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.UpdateSandboxStatus(id, store.SandboxStopped, ""); err != nil {
		t.Fatal(err)
	}
	w := work()
	if got := string(w["retained"]); got != `["`+id+`"]` {
		t.Fatalf("retained=%s", got)
	}
	if got := string(w["assigned"]); got != "[]" {
		t.Fatalf("assigned=%s: a stopped sandbox holds nothing", got)
	}
}

// RBAC: stop, resume and delete are the owner's, or an admin's, or an operator's
// with destroy-any; resume also needs the right to create.
func TestRBACStopResumeDelete(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	key, kid, v := testIdP(t)
	mem := newTestStore(t)
	h := AuthMiddleware(mem, AuthConfig{IdP: v})(testMux(NewServer(mem)))

	alice := mintUserJWTWithGroups(t, key, kid, "user:alice", "", []string{"asp-user"})
	bob := mintUserJWTWithGroups(t, key, kid, "user:bob", "", []string{"asp-user"})
	viewer := mintUserJWTWithGroups(t, key, kid, "user:view", "", []string{"asp-viewer"})
	op := mintUserJWTWithGroups(t, key, kid, "user:op", "", []string{"asp-operator"})
	opAny := mintUserJWTWithGroups(t, key, kid, "user:opany", "", []string{"asp-operator", "sandbox:destroy-any"})
	admin := mintUserJWTWithGroups(t, key, kid, "user:admin", "", []string{"asp-admin"})

	sb := rbacCreate(t, h, alice)
	runSandbox(t, mem, sb.ID)
	stop := "/v1/sandboxes/" + sb.ID + "/stop"
	start := "/v1/sandboxes/" + sb.ID + "/start"

	for name, tok := range map[string]string{"bob": bob, "viewer": viewer, "operator": op} {
		for _, path := range []string{stop, start} {
			if code, body := rbacDo(t, h, tok, http.MethodPost, path, ""); code != http.StatusForbidden {
				t.Fatalf("%s POST %s: want 403, got %d %s", name, path, code, body)
			}
		}
	}
	if code, body := rbacDo(t, h, alice, http.MethodPost, stop, ""); code != http.StatusOK {
		t.Fatalf("alice stops her own: %d %s", code, body)
	}
	if _, err := mem.UpdateSandboxStatus(sb.ID, store.SandboxStopped, ""); err != nil {
		t.Fatal(err)
	}
	if code, body := rbacDo(t, h, alice, http.MethodPost, start, ""); code != http.StatusOK {
		t.Fatalf("alice resumes her own: %d %s", code, body)
	}
	runSandbox(t, mem, sb.ID)
	if code, body := rbacDo(t, h, opAny, http.MethodPost, stop, ""); code != http.StatusOK {
		t.Fatalf("operator with destroy-any stops hers: %d %s", code, body)
	}
	if _, err := mem.UpdateSandboxStatus(sb.ID, store.SandboxStopped, ""); err != nil {
		t.Fatal(err)
	}
	if code, body := rbacDo(t, h, admin, http.MethodPost, start, ""); code != http.StatusOK {
		t.Fatalf("admin resumes any: %d %s", code, body)
	}
	runSandbox(t, mem, sb.ID)
	if code, body := rbacDo(t, h, bob, http.MethodDelete, "/v1/sandboxes/"+sb.ID, ""); code != http.StatusForbidden {
		t.Fatalf("bob deletes alice's: %d %s", code, body)
	}
	if code, body := rbacDo(t, h, alice, http.MethodDelete, "/v1/sandboxes/"+sb.ID, ""); code != http.StatusOK {
		t.Fatalf("alice deletes her own: %d %s", code, body)
	}
}

// A failed resume's detail starts with "resume failed:"; exec's message must not repeat it.
func TestExecMessageDoesNotRepeatResumeFailed(t *testing.T) {
	msg := execBlock(store.Sandbox{State: store.SandboxStopped, StatusDetail: "resume failed: the guest did not answer within 1m0s"})
	if strings.Contains(msg, "resume failed: resume failed") || !strings.Contains(msg, "the last resume failed: the guest did not answer within 1m0s") {
		t.Fatalf("message=%q", msg)
	}
}
