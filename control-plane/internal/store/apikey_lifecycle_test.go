package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGenerateAPIKeySecret(t *testing.T) {
	a, err := GenerateAPIKeySecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateAPIKeySecret()
	if !strings.HasPrefix(a, "asp_") || len(a) != len("asp_")+40 || a == b {
		t.Fatalf("secrets %q %q", a, b)
	}
}

// exerciseAPIKeyLifecycle runs the same story on any store: create, clash,
// list, authenticate, rotate, revoke.
func exerciseAPIKeyLifecycle(t *testing.T, s Store) {
	t.Helper()
	mk := func(name string) (string, string) {
		secret, err := GenerateAPIKeySecret()
		if err != nil {
			t.Fatal(err)
		}
		return secret, KeyPrefix(secret)
	}
	exp := time.Now().Add(time.Hour).UTC()
	sec1, pre1 := mk("a")
	k1, err := s.CreateAPIKey(context.Background(), "acme", "ci", APIKeyScopeTenant, pre1, HashAPIKeySecret(sec1), &exp)
	if err != nil {
		t.Fatal(err)
	}
	if k1.ID == "" || k1.Scope != APIKeyScopeTenant || k1.ExpiresAt == nil || k1.RevokedAt != nil {
		t.Fatalf("created %+v", k1)
	}
	sec2, pre2 := mk("b")
	k2, err := s.CreateAPIKey(context.Background(), "acme", "ops", APIKeyScopePlatform, pre2, HashAPIKeySecret(sec2), nil)
	if err != nil {
		t.Fatal(err)
	}
	sec3, pre3 := mk("c")
	if _, err := s.CreateAPIKey(context.Background(), "other", "ci", "", pre3, HashAPIKeySecret(sec3), nil); err != nil {
		t.Fatalf("same name in another tenant: %v", err)
	}

	// Clashes: the name in the tenant, and the prefix anywhere.
	if _, err := s.CreateAPIKey(context.Background(), "acme", "ci", APIKeyScopeTenant, "xxxxxxxx", "h", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := s.CreateAPIKey(context.Background(), "acme", "fresh", APIKeyScopeTenant, pre1, "h2", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate prefix: %v", err)
	}
	if _, err := s.CreateAPIKey(context.Background(), "acme", "x", "superuser", "pppppppp", "h3", nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad scope: %v", err)
	}

	// Lists never carry secrets, and filter by tenant.
	all, err := s.ListAPIKeys(context.Background(), "")
	if err != nil || len(all) != 3 {
		t.Fatalf("list all: %d %v", len(all), err)
	}
	acme, err := s.ListAPIKeys(context.Background(), "acme")
	if err != nil || len(acme) != 2 {
		t.Fatalf("list acme: %d %v", len(acme), err)
	}
	if got, err := s.GetAPIKey(context.Background(), k2.ID); err != nil || got.Name != "ops" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.GetAPIKey(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get unknown: %v", err)
	}

	// The key authenticates until it is rotated or revoked.
	if got, err := s.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(sec1)); err != nil || got.ID != k1.ID {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	newSec, newPre := mk("d")
	rot, err := s.RotateAPIKey(context.Background(), k1.ID, newPre, HashAPIKeySecret(newSec))
	if err != nil || rot.ID != k1.ID || rot.KeyPrefix != newPre {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	if _, err := s.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(sec1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old secret still works after rotate: %v", err)
	}
	if got, err := s.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(newSec)); err != nil || got.ID != k1.ID {
		t.Fatalf("the new secret does not work: %+v %v", got, err)
	}
	if _, err := s.RotateAPIKey(context.Background(), k1.ID, pre2, "hh"); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotate onto a taken prefix: %v", err)
	}
	if _, err := s.RotateAPIKey(context.Background(), "nope", "qqqqqqqq", "hq"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate unknown: %v", err)
	}

	before, _ := s.CountAPIKeys(context.Background())
	revoked, err := s.RevokeAPIKey(context.Background(), k1.ID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	again, err := s.RevokeAPIKey(context.Background(), k1.ID)
	if err != nil || again.RevokedAt == nil || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("revoking twice moved the revocation: %+v %v", again, err)
	}
	if _, err := s.LookupAPIKeyByHash(context.Background(), HashAPIKeySecret(newSec)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked key still authenticates: %v", err)
	}
	if after, _ := s.CountAPIKeys(context.Background()); after != before-1 {
		t.Fatalf("active keys %d -> %d", before, after)
	}
	if _, err := s.RotateAPIKey(context.Background(), k1.ID, "rrrrrrrr", "hr"); !errors.Is(err, ErrConflict) {
		t.Fatalf("rotating a revoked key: %v", err)
	}
	// A revoked key is still listed, so the audit trail shows it.
	acme, _ = s.ListAPIKeys(context.Background(), "acme")
	found := false
	for _, k := range acme {
		if k.ID == k1.ID && k.RevokedAt != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("the revoked key is not in the list: %+v", acme)
	}
	if _, err := s.RevokeAPIKey(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}
}

func TestMemoryAPIKeyLifecycle(t *testing.T) { exerciseAPIKeyLifecycle(t, NewMemoryStore()) }
