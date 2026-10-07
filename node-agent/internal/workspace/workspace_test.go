package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layout builds <root>/<tenant>/proj and a few places a tenant must not reach.
type layout struct {
	root, outside string
	roots         Roots
}

func newLayout(t *testing.T) layout {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := layout{root: filepath.Join(base, "workspaces"), outside: filepath.Join(base, "outside")}
	for _, d := range []string{
		filepath.Join(l.root, "acme", "proj", "sub"),
		filepath.Join(l.root, "other", "secret"),
		l.outside,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(l.root, "acme", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	l.roots = Roots{l.root}
	return l
}

func TestResolveAllowsATenantsOwnDirectories(t *testing.T) {
	l := newLayout(t)
	for _, p := range []string{
		filepath.Join(l.root, "acme", "proj"),
		filepath.Join(l.root, "acme", "proj", "sub"),
		filepath.Join(l.root, "acme"), // the tenant's whole area
		filepath.Join(l.root, "acme", "proj") + "/",
		filepath.Join(l.root, "acme", "proj", ".", "sub", ".."),
	} {
		got, err := l.roots.Resolve("acme", p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if !filepath.IsAbs(got) || strings.Contains(got, "..") {
			t.Errorf("%s resolved to %q", p, got)
		}
	}
}

func TestResolveRefusesWhatIsNotTheTenants(t *testing.T) {
	l := newLayout(t)
	if err := os.Symlink(l.outside, filepath.Join(l.root, "acme", "link-out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(l.root, "other"), filepath.Join(l.root, "acme", "link-other")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(l.root, "acme", "link-root")); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ tenant, path string }{
		"the root of the node":           {"acme", "/"},
		"the workspace root itself":      {"acme", l.root},
		"another tenant's directory":     {"acme", filepath.Join(l.root, "other", "secret")},
		"another tenant via ..":          {"acme", filepath.Join(l.root, "acme", "..", "other", "secret")},
		"a sibling with the same prefix": {"acme", l.root + "/acmeevil"},
		"outside every root":             {"acme", l.outside},
		"a link out of the tenant dir":   {"acme", filepath.Join(l.root, "acme", "link-out")},
		"a link to another tenant":       {"acme", filepath.Join(l.root, "acme", "link-other")},
		"a link to /":                    {"acme", filepath.Join(l.root, "acme", "link-root")},
		"through a link to /":            {"acme", filepath.Join(l.root, "acme", "link-root", "etc")},
	} {
		got, err := l.roots.Resolve(tc.tenant, tc.path)
		if err == nil {
			t.Errorf("%s: allowed, resolved to %q", name, got)
		}
	}
	// A tenant id that is not one directory name never matches.
	for _, tenant := range []string{"", ".", "..", "a/b", "../other", "acme\x00"} {
		if _, err := l.roots.Resolve(tenant, filepath.Join(l.root, "acme", "proj")); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("tenant %q: %v", tenant, err)
		}
	}
}

func TestResolveNeedsARealDirectory(t *testing.T) {
	l := newLayout(t)
	for name, p := range map[string]string{
		"relative":          "acme/proj",
		"missing":           filepath.Join(l.root, "acme", "nope"),
		"a file":            filepath.Join(l.root, "acme", "file"),
		"empty":             "",
		"a tenant-less dir": filepath.Join(l.root, "nobody"),
	} {
		if _, err := l.roots.Resolve("acme", p); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestNoRootsMeansNoWorkspaces(t *testing.T) {
	l := newLayout(t)
	_, err := Roots(nil).Resolve("acme", filepath.Join(l.root, "acme", "proj"))
	if !errors.Is(err, ErrNotAllowed) || !strings.Contains(err.Error(), "--workspace-root") {
		t.Fatalf("got %v", err)
	}
	// A root that does not exist allows nothing either.
	if _, err := (Roots{filepath.Join(l.root, "missing")}).Resolve("acme", filepath.Join(l.root, "acme", "proj")); err == nil {
		t.Fatal("a missing root allowed a path")
	}
}

func TestSecondRootIsUsed(t *testing.T) {
	l := newLayout(t)
	other := filepath.Join(filepath.Dir(l.root), "more")
	if err := os.MkdirAll(filepath.Join(other, "acme", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots := Roots{l.root, other}
	if _, err := roots.Resolve("acme", filepath.Join(other, "acme", "p")); err != nil {
		t.Fatal(err)
	}
}

func TestParseRoots(t *testing.T) {
	got, err := ParseRoots(" /srv/a/ , /srv/b,, ")
	if err != nil || len(got) != 2 || got[0] != "/srv/a" || got[1] != "/srv/b" {
		t.Fatalf("got %v %v", got, err)
	}
	if got, err := ParseRoots(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"relative/dir", "/", "/srv/ok,rel", "//"} {
		if _, err := ParseRoots(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
