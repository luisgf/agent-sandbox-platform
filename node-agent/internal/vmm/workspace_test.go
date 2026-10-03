package vmm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCreateVMDoesNotPretendVirtiofsWithoutSocket(t *testing.T) {
	var body chVMConfig
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == chPathVMCreate {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	ch := NewCloudHypervisorWithClient("/tmp/fake.sock", client)
	cfg := MicroVMConfig{
		ID: "sb", KernelPath: "/k", RootFSPath: "/r", CPUs: 1, MemoryMiB: 128,
		WorkspaceHostPath: "/data/proj",
	}
	if err := ch.CreateVM(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(body.Fs) != 0 {
		t.Fatalf("fs leaked into vm.create: %+v", body.Fs)
	}
	cfg.WorkspaceFSSocket = "/run/asp/virtiofs.sock"
	if err := ch.CreateVM(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(body.Fs) != 1 || body.Fs[0].Tag != WorkspaceVirtiofsTag || body.Fs[0].Socket != cfg.WorkspaceFSSocket {
		t.Fatalf("fs=%+v", body.Fs)
	}
}

func TestFakeVMMRecordsWorkspace(t *testing.T) {
	f := NewFakeVMM(nil)
	cfg := MicroVMConfig{ID: "sb", CPUs: 1, MemoryMiB: 64, WorkspaceHostPath: "/data/proj"}
	if err := f.Start(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.Running["sb"].WorkspaceHostPath
	f.mu.Unlock()
	if got != "/data/proj" {
		t.Fatalf("recorded=%q", got)
	}
}
