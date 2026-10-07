package store

import (
	"errors"
	"testing"
	"time"
)

// Where the memory store used to accept what Postgres refuses (or the other way
// round) is pinned here for the memory store, which every test run has; the parity
// test (parity_test.go) holds Postgres to the same answers where DATABASE_URL names
// a server.

func TestNodeNamesAreUnique(t *testing.T) {
	s := NewMemoryStore()
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n1", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n2", Name: "alpha"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second node named alpha: want ErrConflict, got %v", err)
	}
	if _, ok := s.nodes["n2"]; ok {
		t.Fatal("the refused node was stored")
	}
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n1", Name: "alpha", CapacityCPU: 2}); err != nil {
		t.Fatalf("a node registering again under its own name: %v", err)
	}
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n2", Name: "beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n2", Name: "alpha"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("renaming n2 to alpha: want ErrConflict, got %v", err)
	}
}

func TestEnrollingUnderATakenNameKeepsTheToken(t *testing.T) {
	s := NewMemoryStore()
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n1", Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	hash := HashEnrollToken("tok")
	if err := s.CreateEnrollToken(bg, EnrollToken{Hash: hash, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cert := CertMeta{Fingerprint: "fp"}
	if _, err := s.EnrollNode(bg, EnrollNodeInput{ID: "n2", Name: "alpha"}, cert, EnrollAuth{TokenHash: hash}); !errors.Is(err, ErrConflict) {
		t.Fatalf("enrolling n2 as alpha: want ErrConflict, got %v", err)
	}
	// The enrollment failed as a whole, as a rolled-back transaction would: the
	// token was not spent.
	if _, err := s.EnrollNode(bg, EnrollNodeInput{ID: "n2", Name: "gamma"}, cert, EnrollAuth{TokenHash: hash}); err != nil {
		t.Fatalf("the token must still work: %v", err)
	}
}

func TestEventsOfSomethingThatDoesNotExistAreRefused(t *testing.T) {
	s := NewMemoryStore()
	if err := s.EmitEvent(bg, EmitEventInput{SandboxID: "nope", TenantID: "t", EventType: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("event of an unknown sandbox: want ErrNotFound, got %v", err)
	}
	if evs, _ := s.ListEvents(bg, "nope"); len(evs) != 0 {
		t.Fatalf("the refused event was stored: %+v", evs)
	}
	if err := s.EmitNodeEvent(bg, "nope", "x", "a", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("event of an unknown node: want ErrNotFound, got %v", err)
	}
	if _, err := s.RegisterNode(bg, RegisterNodeInput{ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EmitNodeEvent(bg, "n1", "x", "a", nil); err != nil {
		t.Fatalf("event of a known node: %v", err)
	}
}

func TestAPIKeysNeverShareAPrefixOrASecret(t *testing.T) {
	s := NewMemoryStore()
	if _, err := s.CreateAPIKey(bg, "t1", "ci", APIKeyScopeTenant, "pre00001", "hash-1", nil); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"create with a taken prefix": func() error {
			_, e := s.CreateAPIKey(bg, "t2", "a", APIKeyScopeTenant, "pre00001", "hash-2", nil)
			return e
		},
		"create with a taken secret": func() error {
			_, e := s.CreateAPIKey(bg, "t2", "a", APIKeyScopeTenant, "pre00002", "hash-1", nil)
			return e
		},
		"ensure with a taken prefix": func() error { _, e := s.EnsureAPIKey(bg, "t2", "a", APIKeyScopeTenant, "pre00001", "hash-2"); return e },
		"ensure with a taken secret": func() error { _, e := s.EnsureAPIKey(bg, "t2", "a", APIKeyScopeTenant, "pre00002", "hash-1"); return e },
	} {
		if err := call(); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: want ErrConflict, got %v", name, err)
		}
	}
	other, err := s.CreateAPIKey(bg, "t2", "b", APIKeyScopeTenant, "pre00003", "hash-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateAPIKey(bg, other.ID, "pre00004", "hash-1"); !errors.Is(err, ErrConflict) {
		t.Errorf("rotate to a taken secret: want ErrConflict, got %v", err)
	}
	if _, err := s.RotateAPIKey(bg, other.ID, "pre00003", "hash-3"); err != nil {
		t.Errorf("rotate to its own prefix and secret: %v", err)
	}
	// Neither refusal lost the key that already had the secret.
	if k, err := s.LookupAPIKeyByHash(bg, "hash-1"); err != nil || k.Name != "ci" {
		t.Fatalf("the first key: %+v %v", k, err)
	}
}

// A revocation holds while the same secret is ensured again (a restart), and ends
// when the operator gives the key another one.
func TestEnsureAPIKeyRevocation(t *testing.T) {
	s := NewMemoryStore()
	k, err := s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0001", "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeAPIKey(bg, k.ID); err != nil {
		t.Fatal(err)
	}
	same, err := s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0001", "hash-a")
	if err != nil || same.RevokedAt == nil {
		t.Fatalf("the same secret must stay revoked: %+v %v", same, err)
	}
	if _, err := s.LookupAPIKeyByHash(bg, "hash-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked key authenticates: %v", err)
	}
	back, err := s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0002", "hash-b")
	if err != nil || back.RevokedAt != nil || back.ID != k.ID {
		t.Fatalf("a new secret brings the key back: %+v %v", back, err)
	}
	if _, err := s.LookupAPIKeyByHash(bg, "hash-b"); err != nil {
		t.Fatalf("the new secret: %v", err)
	}
	if _, err := s.LookupAPIKeyByHash(bg, "hash-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old secret must not work: %v", err)
	}
}

func TestEgressRulesAreListedByHostThenPort(t *testing.T) {
	s := NewMemoryStore()
	p443, p80 := 443, 80
	got, err := s.PutEgressRules(bg, "t", []EgressRule{
		{HostPattern: "registry.npmjs.org", Port: &p443, Enabled: true},
		{HostPattern: "registry.npmjs.org", Port: &p80, Enabled: true},
		{HostPattern: "registry.npmjs.org", Enabled: true},
		{HostPattern: "*.github.com", Enabled: true},
		{HostPattern: "api.example.com", Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"*.github.com", "api.example.com", "registry.npmjs.org", "registry.npmjs.org", "registry.npmjs.org"}
	ports := []int{-1, -1, -1, 80, 443}
	for i, r := range got {
		p := -1
		if r.Port != nil {
			p = *r.Port
		}
		if r.HostPattern != want[i] || p != ports[i] {
			t.Fatalf("rule %d is %s:%d, want %s:%d (all: %+v)", i, r.HostPattern, p, want[i], ports[i], got)
		}
	}
	listed, _ := s.ListEgressRules(bg, "t")
	for i := range listed {
		if listed[i].HostPattern != got[i].HostPattern {
			t.Fatalf("a list is not in the order of the put: %+v vs %+v", listed, got)
		}
	}
}
