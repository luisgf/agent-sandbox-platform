package store

import (
	"context"
	"errors"
	"testing"
)

// Node scheduling attributes must behave the same in both stores.
func testNodeSchedulingAttributes(t *testing.T, s Store) {
	t.Helper()
	no := false
	n, err := s.RegisterNode(context.Background(), RegisterNodeInput{
		ID: "n1", AgentEndpoint: "http://127.0.0.1:9100",
		CapacityCPU: 8, CapacityMemMiB: 16384, MaxSandboxes: 3,
		AcceptsWork: &no, LocalNetDial: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.MaxSandboxes != 3 || n.AcceptsWork || n.LocalNetDial != "203.0.113.10" || n.Cordoned {
		t.Fatalf("register attributes: %+v", n)
	}

	// Agents that predate accepts_work send nothing: they accept work.
	n, err = s.RegisterNode(context.Background(), RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100", CapacityCPU: 8, CapacityMemMiB: 16384})
	if err != nil {
		t.Fatal(err)
	}
	if !n.AcceptsWork || n.MaxSandboxes != 0 || n.LocalNetDial != "" {
		t.Fatalf("re-register replaces attributes: %+v", n)
	}

	for _, in := range []RegisterNodeInput{
		{ID: "bad", CapacityCPU: -1},
		{ID: "bad", CapacityMemMiB: -1},
		{ID: "bad", MaxSandboxes: -1},
	} {
		if _, err := s.RegisterNode(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("register %+v: want ErrInvalidInput, got %v", in, err)
		}
	}

	// Enroll does not carry scheduling attributes: re-enroll keeps them, a new node accepts work.
	if _, err := s.RegisterNode(context.Background(), RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100", MaxSandboxes: 5, LocalNetDial: "198.51.100.7:51820"}); err != nil {
		t.Fatal(err)
	}
	n, err = s.EnrollNode(context.Background(), EnrollNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}, CertMeta{Fingerprint: "fp-n1"}, EnrollAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if n.MaxSandboxes != 5 || n.LocalNetDial != "198.51.100.7:51820" || !n.AcceptsWork {
		t.Fatalf("re-enroll lost scheduling attributes: %+v", n)
	}
	n, err = s.EnrollNode(context.Background(), EnrollNodeInput{ID: "n2", AgentEndpoint: "http://127.0.0.1:9101"}, CertMeta{Fingerprint: "fp-n2"}, EnrollAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if !n.AcceptsWork || n.Cordoned || n.MaxSandboxes != 0 {
		t.Fatalf("new enrolled node defaults: %+v", n)
	}
}

func TestMemoryNodeSchedulingAttributes(t *testing.T) {
	testNodeSchedulingAttributes(t, NewMemoryStore())
}

func TestPostgresNodeSchedulingAttributes(t *testing.T) {
	testNodeSchedulingAttributes(t, newPostgresTestStore(t))
}

// A node says on every register whether it forces its guests through its egress
// proxy; the control plane keeps the latest answer, an older node (which sends
// nothing) is not enforcing, and enrolling does not change it.
func testNodeEgressEnforced(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	n, err := s.RegisterNode(ctx, RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100", EgressEnforced: true})
	if err != nil || !n.EgressEnforced {
		t.Fatalf("register enforcing: %+v %v", n, err)
	}
	got, err := s.GetNode(ctx, "n1")
	if err != nil || !got.EgressEnforced {
		t.Fatalf("stored: %+v %v", got, err)
	}
	n, err = s.EnrollNode(ctx, EnrollNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}, CertMeta{Fingerprint: "fp-n1"}, EnrollAuth{})
	if err != nil || !n.EgressEnforced {
		t.Fatalf("enroll changed it: %+v %v", n, err)
	}
	n, err = s.RegisterNode(ctx, RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}) // an older agent, or one that lost the rules
	if err != nil || n.EgressEnforced {
		t.Fatalf("register not enforcing: %+v %v", n, err)
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil || len(nodes) == 0 || nodes[0].EgressEnforced {
		t.Fatalf("list: %+v %v", nodes, err)
	}
}

func TestMemoryNodeEgressEnforced(t *testing.T)   { testNodeEgressEnforced(t, NewMemoryStore()) }
func TestPostgresNodeEgressEnforced(t *testing.T) { testNodeEgressEnforced(t, newPostgresTestStore(t)) }
