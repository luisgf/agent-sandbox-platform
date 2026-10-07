package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var out, errb bytes.Buffer
		if code := run([]string{arg}, &out, &errb); code != 0 {
			t.Fatalf("asp %s: exit %d: %s", arg, code, errb.String())
		}
		if !strings.HasPrefix(out.String(), "asp dev") || !strings.Contains(out.String(), "go1") {
			t.Fatalf("asp %s printed %q", arg, out.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"version", "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var info struct {
		Version   string `json:"version"`
		GoVersion string `json:"go_version"`
	}
	if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.Version == "" || info.GoVersion == "" {
		t.Fatalf("version --json printed %q: %v", out.String(), err)
	}
	if code := run([]string{"version", "--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("a bad flag: exit %d, want 2", code)
	}
}
