package attest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"
)

func TestSoftwareAttestVerifyRoundTrip(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := NewSoftwareAttestorFromKey(key)
	stmt := BootStatement{
		SandboxID:   "sb-1",
		ImageDigest: "sha256:abc",
		VMMProfile:  "cloud-hypervisor",
		CID:         3,
		NodeID:      "node-a",
		TS:          time.Now().UTC().Format(time.RFC3339),
	}
	ev, err := a.Attest(context.Background(), stmt)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Alg != AlgES256 || ev.Signature == "" || ev.KeyID == "" {
		t.Fatalf("evidence=%+v", ev)
	}
	if err := a.Verify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if !a.IsFresh(ev, time.Now().UTC()) {
		t.Fatal("expected fresh")
	}
}

func TestSoftwareAttestStale(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := NewSoftwareAttestorFromKey(key)
	a.MaxAge = time.Second
	stmt := BootStatement{
		SandboxID: "sb-1",
		NodeID:    "node-a",
		TS:        time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	ev, err := a.Attest(context.Background(), stmt)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(context.Background(), ev); err == nil {
		t.Fatal("expected stale error")
	}
	if a.IsFresh(ev, time.Now().UTC()) {
		t.Fatal("should not be fresh")
	}
}

func TestSoftwareAttestBadSig(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := NewSoftwareAttestorFromKey(key)
	stmt := BootStatement{
		SandboxID: "sb-1",
		NodeID:    "node-a",
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
	ev, err := a.Attest(context.Background(), stmt)
	if err != nil {
		t.Fatal(err)
	}
	ev.Signature = "AAAA" + ev.Signature[4:]
	if err := a.Verify(context.Background(), ev); err == nil {
		t.Fatal("expected bad sig")
	}
}

func TestAttestorInterface(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var _ Attestor = NewSoftwareAttestorFromKey(key)
}
