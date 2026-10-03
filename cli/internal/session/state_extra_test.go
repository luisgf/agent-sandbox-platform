package session

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNamedPathAndRejectTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ASP_SESSION_DIR", dir)
	if got := DefaultDir(); got != dir {
		t.Fatalf("DefaultDir=%q", got)
	}
	p, err := NamedPath("", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, "agent-a.json") {
		t.Fatalf("path=%s", p)
	}
	def, err := NamedPath(dir, "")
	if err != nil || !strings.HasSuffix(def, "default.json") {
		t.Fatalf("default path=%s err=%v", def, err)
	}
	for _, bad := range []string{"../x", "a/b", "..", "has space", ".hidden"} {
		if _, err := ValidateName(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := Save(p, State{Name: "agent-a", SandboxID: "sb", CPURL: "http://cp"}); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil || st.Name != "agent-a" {
		t.Fatalf("loaded=%+v err=%v", st, err)
	}
}
