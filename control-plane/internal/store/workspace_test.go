package store

import "testing"

func TestWorkspaceHostPathStoredAndValidated(t *testing.T) {
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 64,
		WorkspaceHostPath: "/var/tmp/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.WorkspaceHostPath != "/var/tmp/proj" {
		t.Fatalf("path=%q", sb.WorkspaceHostPath)
	}
	got, err := s.GetSandbox(sb.ID)
	if err != nil || got.WorkspaceHostPath != "/var/tmp/proj" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	if _, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 64,
		WorkspaceHostPath: "relative/proj",
	}); err == nil {
		t.Fatal("expected relative path rejection")
	}
}
