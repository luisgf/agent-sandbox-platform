package store

import (
	"context"
	"testing"
)

func TestWorkspaceHostPathStoredAndValidated(t *testing.T) {
	s := newMemoryStoreWithNodes(t)
	sb, err := s.CreateSandbox(context.Background(), CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 64,
		WorkspaceHostPath: "/var/tmp/proj",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.WorkspaceHostPath != "/var/tmp/proj" {
		t.Fatalf("path=%q", sb.WorkspaceHostPath)
	}
	got, err := s.GetSandbox(context.Background(), sb.ID)
	if err != nil || got.WorkspaceHostPath != "/var/tmp/proj" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	if _, err := s.CreateSandbox(context.Background(), CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 64,
		WorkspaceHostPath: "relative/proj",
	}); err == nil {
		t.Fatal("expected relative path rejection")
	}
}
