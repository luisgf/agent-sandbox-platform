package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadClearRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asp", "session.json")
	st := State{
		SandboxID: "sb-42",
		CPURL:     "http://127.0.0.1:8080/",
		TenantID:  "tenant-demo",
		ImageRef:  "debian:bookworm-slim",
		CreatedAt: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC),
	}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode=%o want 0600", got)
	}
	dirFi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirFi.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode=%o want 0700", got)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SandboxID != "sb-42" || got.CPURL != "http://127.0.0.1:8080" || got.TenantID != "tenant-demo" {
		t.Fatalf("loaded=%+v", got)
	}
	if !got.CreatedAt.Equal(st.CreatedAt) {
		t.Fatalf("created_at=%s", got.CreatedAt)
	}
	// Must not look like a credential store.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ToLower(string(raw))
	for _, needle := range []string{"bearer", "api_key", "id_token", "password", "secret"} {
		if strings.Contains(s, needle) {
			t.Fatalf("session file contains %q:\n%s", needle, s)
		}
	}
	if err := Clear(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after clear: %v", err)
	}
	if err := Clear(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingAndGarbage(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	if _, err := Load(missing); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || errors.Is(err, ErrNoSession) {
		t.Fatalf("garbage err=%v", err)
	}
	emptyID := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyID, []byte(`{"sandbox_id":"  ","cp_url":"http://x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(emptyID); err == nil {
		t.Fatal("expected empty sandbox_id error")
	}
}

func TestSaveRejectsEmptyIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := Save(path, State{CPURL: "http://x"}); err == nil {
		t.Fatal("expected empty id error")
	}
	if err := Save(path, State{SandboxID: "id"}); err == nil {
		t.Fatal("expected empty cp error")
	}
}

func TestDefaultPathEnvOverride(t *testing.T) {
	t.Setenv("ASP_SESSION_FILE", "/tmp/custom-asp-session.json")
	if got := DefaultPath(); got != "/tmp/custom-asp-session.json" {
		t.Fatalf("got %q", got)
	}
}
