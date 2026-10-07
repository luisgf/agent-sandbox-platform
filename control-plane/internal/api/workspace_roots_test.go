package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckWorkspacePath(t *testing.T) {
	roots := []string{"/srv/asp/workspaces", "/data/ws"}
	for _, tc := range []struct {
		tenant, path string
		ok           bool
	}{
		{"acme", "/srv/asp/workspaces/acme/proj", true},
		{"acme", "/srv/asp/workspaces/acme", true},
		{"acme", "/srv/asp/workspaces/acme/a/b/c/", true},
		{"acme", "/data/ws/acme/proj", true},
		{"acme", "", true}, // no workspace asked
		{"acme", "/", false},
		{"acme", "/srv/asp/workspaces", false},
		{"acme", "/srv/asp/workspaces/rival/proj", false},
		{"acme", "/srv/asp/workspaces/acme/../rival/proj", false},
		{"acme", "/srv/asp/workspaces/acmeevil", false},
		{"acme", "/etc", false},
		{"acme", "/home/ubuntu", false},
		{"", "/srv/asp/workspaces/acme", false},
		{"..", "/srv/asp/workspaces/../x", false},
		{"a/b", "/srv/asp/workspaces/a/b/x", false},
	} {
		err := checkWorkspacePath(roots, tc.tenant, tc.path)
		if (err == nil) != tc.ok {
			t.Errorf("tenant %q path %q: err = %v, want ok=%v", tc.tenant, tc.path, err, tc.ok)
		}
	}
	// No roots configured: no early check (the node decides).
	if err := checkWorkspacePath(nil, "acme", "/etc"); err != nil {
		t.Fatalf("with no roots: %v", err)
	}
}

func TestCreateRefusesAWorkspaceOutsideTheRoots(t *testing.T) {
	t.Setenv(EnvWorkspaceRoots, "/srv/asp/workspaces")
	mem, h := newLifecycleFixture(t)
	create := func(path string) *httptest.ResponseRecorder {
		body := `{"tenant_id":"acme","image_ref":"img","cpu_millis":100,"memory_mib":64,"workspace_host_path":"` + path + `"}`
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(body)))
		return rr
	}
	for _, bad := range []string{"/", "/etc", "/srv/asp/workspaces", "/srv/asp/workspaces/rival/p", "/srv/asp/workspaces/acme/../rival/p"} {
		rr := create(bad)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "/srv/asp/workspaces/acme") {
			t.Errorf("workspace %q: %d %s", bad, rr.Code, rr.Body.String())
		}
	}
	if list, _ := mem.ListSandboxes("acme"); len(list) != 0 {
		t.Fatalf("a refused create left %d sandboxes", len(list))
	}
	if rr := create("/srv/asp/workspaces/acme/proj"); rr.Code != http.StatusCreated {
		t.Fatalf("an allowed workspace: %d %s", rr.Code, rr.Body.String())
	}
	// And a create without a workspace is not affected.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/sandboxes", bytes.NewBufferString(`{"tenant_id":"acme","image_ref":"img","cpu_millis":100,"memory_mib":64}`)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("no workspace: %d %s", rr.Code, rr.Body.String())
	}
}
