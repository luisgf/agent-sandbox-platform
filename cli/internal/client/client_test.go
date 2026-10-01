package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientCRUDAndExec(t *testing.T) {
	var created Sandbox
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth=%q", got)
		}
		var in CreateInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		nid := in.NodeID
		created = Sandbox{
			ID: "sb-1", TenantID: in.TenantID, State: "requested",
			ImageRef: in.ImageRef, CPUMillis: in.CPUMillis, MemoryMiB: in.MemoryMiB,
		}
		if nid != "" {
			created.NodeID = &nid
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(created)
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		sb := created
		sb.State = "running"
		_ = json.NewEncoder(w).Encode(sb)
	})
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tenant_id") != "t1" {
			t.Errorf("tenant=%q", r.URL.Query().Get("tenant_id"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []Sandbox{created}})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		sb := created
		sb.State = "stopping"
		_ = json.NewEncoder(w).Encode(sb)
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var req ExecRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Cmd) < 1 || req.Cmd[0] != "echo" {
			t.Errorf("cmd=%v", req.Cmd)
		}
		_ = json.NewEncoder(w).Encode(ExecResult{Stdout: "hello\n", ExitCode: 0})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "test-key")
	ctx := context.Background()
	sb, err := c.CreateSandbox(ctx, CreateInput{TenantID: "t1", ImageRef: "img", CPUMillis: 1, MemoryMiB: 1, NodeID: "n1"})
	if err != nil || sb.ID != "sb-1" {
		t.Fatalf("create: %v %#v", err, sb)
	}
	got, err := c.GetSandbox(ctx, "sb-1")
	if err != nil || got.State != "running" {
		t.Fatalf("get: %v %#v", err, got)
	}
	list, err := c.ListSandboxes(ctx, "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", err, list)
	}
	res, err := c.Exec(ctx, "sb-1", ExecRequest{Cmd: []string{"echo", "hello"}})
	if err != nil || res.ExitCode != 0 || res.Stdout != "hello\n" {
		t.Fatalf("exec: %v %#v", err, res)
	}
	dsb, err := c.DeleteSandbox(ctx, "sb-1")
	if err != nil || dsb.State != "stopping" {
		t.Fatalf("delete: %v %#v", err, dsb)
	}
}

func TestClientHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"sandbox not found"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	_, err := c.GetSandbox(context.Background(), "missing")
	he, ok := err.(*HTTPError)
	if !ok || he.StatusCode != 404 || he.Message != "sandbox not found" {
		t.Fatalf("err=%v", err)
	}
}

func TestClientBearerPrefersIDToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Sandbox{ID: "x", State: "requested"})
	}))
	defer srv.Close()
	c := New(srv.URL, "api-key")
	c.SetBearer("idp-jwt")
	_, err := c.CreateSandbox(context.Background(), CreateInput{TenantID: "t", ImageRef: "i", CPUMillis: 1, MemoryMiB: 1})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer idp-jwt" {
		t.Fatalf("auth=%q", gotAuth)
	}
}
