package rundir

import (
	"os"
	"path/filepath"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestEnsureCreatesPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "asp")
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("new dir mode = %o, want 700", m)
	}
}

func TestEnsureTightensADedicatedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "asp")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("existing dir mode = %o, want 700", m)
	}
}

func TestEnsureLeavesSharedDirectoriesAlone(t *testing.T) {
	// A sticky or world-writable directory is not ours to restrict.
	sticky := filepath.Join(t.TempDir(), "sticky")
	if err := os.Mkdir(sticky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(sticky); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, sticky); m != 0o777 {
		t.Fatalf("world-writable dir changed to %o", m)
	}
	// So are the directories the system uses, however they got named.
	for _, d := range []string{"/", "/run", "/tmp", os.TempDir()} {
		if _, err := os.Stat(d); err != nil {
			continue // /run does not exist on macOS
		}
		before := mode(t, d)
		if err := Ensure(d); err != nil {
			t.Fatalf("Ensure(%s): %v", d, err)
		}
		if after := mode(t, d); after != before {
			t.Fatalf("Ensure(%s) changed its mode from %o to %o", d, before, after)
		}
	}
}

func TestEnsureRefusesAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(f); err == nil {
		t.Fatal("a regular file is not a socket directory")
	}
}

func TestEnsureSearchableLetsOthersThroughButNotIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run", "asp-vm")
	if err := EnsureSearchable(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != SearchableMode {
		t.Fatalf("new dir mode = %o, want 711", m)
	}
	// An existing directory that others can list, or that they cannot enter, is
	// brought to the same mode.
	for _, before := range []os.FileMode{0o755, 0o700, 0o750} {
		d := filepath.Join(t.TempDir(), "d")
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, before); err != nil {
			t.Fatal(err)
		}
		if err := EnsureSearchable(d); err != nil {
			t.Fatal(err)
		}
		if m := mode(t, d); m != SearchableMode {
			t.Fatalf("mode %o became %o, want 711", before, m)
		}
	}
	// Shared directories are not ours to restrict, and they let everyone through.
	for _, d := range []string{"/", "/tmp", os.TempDir()} {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		before := mode(t, d)
		if err := EnsureSearchable(d); err != nil {
			t.Fatalf("EnsureSearchable(%s): %v", d, err)
		}
		if after := mode(t, d); after != before {
			t.Fatalf("EnsureSearchable(%s) changed its mode from %o to %o", d, before, after)
		}
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSearchable(f); err == nil {
		t.Fatal("a regular file is not a directory to search")
	}
}
