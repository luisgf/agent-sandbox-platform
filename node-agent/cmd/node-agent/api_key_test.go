package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadNodeAPIKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	if err := os.WriteFile(file, []byte("  platform-key-from-file \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASP_NODE_API_KEY", "key-from-env")
	if got, err := loadNodeAPIKey(config{APIKeyFile: file}); err != nil || got != "platform-key-from-file" {
		t.Fatalf("file: %q %v", got, err)
	}
	if got, err := loadNodeAPIKey(config{}); err != nil || got != "key-from-env" {
		t.Fatalf("env: %q %v", got, err)
	}
	t.Setenv("ASP_NODE_API_KEY", "")
	if got, err := loadNodeAPIKey(config{}); err != nil || got != "" {
		t.Fatalf("none: %q %v", got, err)
	}
	for _, bad := range []string{filepath.Join(dir, "missing"), empty} {
		if _, err := loadNodeAPIKey(config{APIKeyFile: bad}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
