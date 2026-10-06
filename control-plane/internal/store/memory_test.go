package store

import (
	"errors"
	"sync"
	"testing"
)

func withAutoProvision(t *testing.T) {
	t.Helper()
	t.Setenv("ASP_AUTO_PROVISION", "1")
}

func TestMemoryStoreCreateGetList(t *testing.T) {
	withAutoProvision(t)
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID:  "tenant-a",
		ImageRef:  "debian:bookworm-slim",
		CPUMillis: 1000,
		MemoryMiB: 512,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.ID == "" {
		t.Fatal("expected UUID id")
	}
	if sb.State != SandboxRunning {
		t.Fatalf("expected running after provision stub, got %s", sb.State)
	}
	if sb.NodeID == nil || *sb.NodeID != DefaultLocalNodeID {
		t.Fatalf("expected node_id %q, got %v", DefaultLocalNodeID, sb.NodeID)
	}
	if sb.VMMProfile != "cloud-hypervisor" {
		t.Fatalf("default vmm_profile: got %q", sb.VMMProfile)
	}

	got, err := s.GetSandbox(sb.ID)
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if got.ID != sb.ID || got.TenantID != "tenant-a" {
		t.Fatalf("unexpected sandbox: %+v", got)
	}

	_, err = s.CreateSandbox(CreateSandboxInput{
		TenantID:  "tenant-b",
		ImageRef:  "img",
		CPUMillis: 500,
		MemoryMiB: 256,
	})
	if err != nil {
		t.Fatal(err)
	}

	listA, err := s.ListSandboxes("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 {
		t.Fatalf("tenant-a list len=%d", len(listA))
	}
	all, err := s.ListSandboxes("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all list len=%d", len(all))
	}
}

func TestMemoryStoreEventsOnCreate(t *testing.T) {
	withAutoProvision(t)
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID:  "t",
		ImageRef:  "img",
		CPUMillis: 100,
		MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.ListEvents(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) < 3 {
		t.Fatalf("want >=3 events (created+starting+running), got %d: %+v", len(ev), ev)
	}
	if ev[0].EventType != "sandbox.created" {
		t.Fatalf("first event=%s", ev[0].EventType)
	}
	last := ev[len(ev)-1]
	if last.ToState == nil || *last.ToState != string(SandboxRunning) {
		t.Fatalf("last to_state=%v", last.ToState)
	}
}

func TestMemoryStoreValidation(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.CreateSandbox(CreateSandboxInput{TenantID: "", ImageRef: "x", CPUMillis: 1, MemoryMiB: 64})
	if err == nil {
		t.Fatal("expected validation error")
	}
	_, err = s.GetSandbox("missing")
	if err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMemoryStoreRegisterNode(t *testing.T) {
	s := NewMemoryStore()
	n, err := s.RegisterNode(RegisterNodeInput{
		ID:             "node-1",
		Name:           "dev",
		Endpoint:       "http://127.0.0.1:9090",
		CapacityCPU:    8,
		CapacityMemMiB: 16384,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.State != "ready" {
		t.Fatalf("state=%s", n.State)
	}
	list, err := s.ListNodes()
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	n2, err := s.RegisterNode(RegisterNodeInput{
		ID:             "node-1",
		Name:           "dev",
		Endpoint:       "http://127.0.0.1:9091",
		CapacityCPU:    16,
		CapacityMemMiB: 32768,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n2.Endpoint != "http://127.0.0.1:9091" || n2.CapacityCPU != 16 {
		t.Fatalf("update failed: %+v", n2)
	}
	if n2.CreatedAt != n.CreatedAt {
		t.Fatal("CreatedAt should be preserved on re-register")
	}
}

func TestMemoryStoreAPIKey(t *testing.T) {
	s := NewMemoryStore()
	secret := "asp_test_secret_value"
	hash := HashAPIKeySecret(secret)
	k, err := s.EnsureAPIKey("default", "bootstrap", APIKeyScopePlatform, KeyPrefix(secret), hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LookupAPIKeyByHash(hash)
	if err != nil || got.ID != k.ID {
		t.Fatalf("lookup: %+v err=%v", got, err)
	}
	n, err := s.CountAPIKeys()
	if err != nil || n != 1 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	// Ensure is idempotent by name
	k2, err := s.EnsureAPIKey("default", "bootstrap", APIKeyScopePlatform, KeyPrefix(secret), hash)
	if err != nil || k2.ID != k.ID {
		t.Fatalf("ensure again: %+v", k2)
	}
}

func TestMemoryStoreConcurrentCreate(t *testing.T) {
	withAutoProvision(t)
	s := NewMemoryStore()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := s.CreateSandbox(CreateSandboxInput{
				TenantID:  "t",
				ImageRef:  "img",
				CPUMillis: 100,
				MemoryMiB: 128,
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.ListSandboxes("")
	if len(all) != n {
		t.Fatalf("want %d sandboxes, got %d", n, len(all))
	}
}

func TestMemoryStoreEnrollAndHeartbeat(t *testing.T) {
	withAutoProvision(t)
	s := NewMemoryStore()
	node, err := s.EnrollNode(EnrollNodeInput{
		ID: "n-enroll", Name: "n-enroll",
		AgentEndpoint: "http://127.0.0.1:9100",
		CapacityCPU:   1, CapacityMemMiB: 512,
	}, CertMeta{Fingerprint: "abc123fingerprint", Serial: "aa"}, EnrollAuth{})
	if err != nil {
		t.Fatal(err)
	}
	if node.CertFingerprint != "abc123fingerprint" || node.CertSerial != "aa" || node.EnrolledAt == nil {
		t.Fatalf("enroll fields: %+v", node)
	}
	hb, err := s.HeartbeatNode("n-enroll")
	if err != nil {
		t.Fatal(err)
	}
	if hb.LastSeenAt == nil {
		t.Fatal("expected last_seen")
	}
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 64, NodeID: "n-enroll",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.NodeID == nil || *sb.NodeID != "n-enroll" {
		t.Fatalf("node pin failed: %+v", sb)
	}
}

// newMemoryStoreWithNodes returns a store with healthy nodes that can take
// sandboxes: since ADR-0011 a create without a schedulable node is refused.
func newMemoryStoreWithNodes(t *testing.T, ids ...string) *MemoryStore {
	t.Helper()
	s := NewMemoryStore()
	if len(ids) == 0 {
		ids = []string{"test-node"}
	}
	for _, id := range ids {
		if _, err := s.RegisterNode(RegisterNodeInput{ID: id, AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestMemoryStoreCreateWithoutNodesHasNoCapacity(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := NewMemoryStore()
	_, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128,
	})
	if !errors.Is(err, ErrNoCapacity) || err.Error() != "no schedulable nodes registered" {
		t.Fatalf("want no capacity, got %v", err)
	}
	if list, _ := s.ListSandboxes(""); len(list) != 0 {
		t.Fatalf("a refused create must not leave a sandbox: %+v", list)
	}
}

func TestMemoryStorePlacesOnCreate(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := NewMemoryStore()
	_, err := s.RegisterNode(RegisterNodeInput{
		ID: "n1", Name: "n1", Endpoint: "http://127.0.0.1:9",
		AgentEndpoint: "http://127.0.0.1:9100", CapacityCPU: 1, CapacityMemMiB: 512,
	})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != SandboxRequested {
		t.Fatalf("state=%s", sb.State)
	}
	if sb.NodeID == nil || *sb.NodeID != "n1" {
		t.Fatalf("soft assign failed: %+v", sb)
	}
}

func TestMemoryStoreClaimAtomicity(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := newMemoryStoreWithNodes(t, "node-0")
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The assigned node races itself (e.g. two reconcile ticks): one claim wins.
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	wins := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if got, err := s.ClaimSandbox(sb.ID, "node-0"); err == nil {
				wins <- *got.NodeID
			}
		}()
	}
	wg.Wait()
	close(wins)
	var winners []string
	for w := range wins {
		winners = append(winners, w)
	}
	if len(winners) != 1 {
		t.Fatalf("want exactly 1 successful claim, got %d: %v", len(winners), winners)
	}
	got, err := s.GetSandbox(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != SandboxStarting || got.NodeID == nil || *got.NodeID != "node-0" {
		t.Fatalf("after claim: %+v", got)
	}
	if _, err := s.ClaimSandbox(sb.ID, "other"); err == nil {
		t.Fatal("expected conflict")
	}
}

func TestMemoryStoreDestroyAndStatus(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := newMemoryStoreWithNodes(t, "n1")
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 64, NodeID: "n1",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimSandbox(sb.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != SandboxStarting {
		t.Fatalf("claim state=%s", claimed.State)
	}
	running, err := s.UpdateSandboxStatus(sb.ID, SandboxRunning, "booted")
	if err != nil || running.State != SandboxRunning {
		t.Fatalf("status running: %+v err=%v", running, err)
	}
	stopping, err := s.MarkSandboxStopping(sb.ID, "")
	if err != nil || stopping.State != SandboxStopping {
		t.Fatalf("destroy: %+v err=%v", stopping, err)
	}
	stopped, err := s.UpdateSandboxStatus(sb.ID, SandboxStopped, "cleaned")
	if err != nil || stopped.State != SandboxStopped {
		t.Fatalf("stopped: %+v err=%v", stopped, err)
	}
	work, err := s.ListNodeWork("n1")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range work.Sandboxes {
		if w.ID == sb.ID {
			t.Fatalf("stopped sandbox still in work: %+v", w)
		}
	}
	for _, id := range work.Assigned {
		if id == sb.ID {
			t.Fatalf("stopped sandbox still assigned to the node")
		}
	}
}

func TestMemoryStoreRotateAndRevokeCert(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.EnrollNode(EnrollNodeInput{
		ID: "n-rot", Name: "n-rot", AgentEndpoint: "http://127.0.0.1:9",
	}, CertMeta{Fingerprint: "fp-old", Serial: "s1"}, EnrollAuth{})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.RotateNodeCert("n-rot", CertMeta{Fingerprint: "fp-new", Serial: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if node.CertFingerprint != "fp-new" || node.CertSerial != "s2" {
		t.Fatalf("rotate: %+v", node)
	}
	revoked, err := s.IsCertRevoked("fp-old")
	if err != nil || !revoked {
		t.Fatalf("old fp should be revoked: revoked=%v err=%v", revoked, err)
	}
	revoked, err = s.IsCertRevoked("fp-new")
	if err != nil || revoked {
		t.Fatalf("new fp should be live: revoked=%v err=%v", revoked, err)
	}
	node, err = s.RevokeNode("n-rot")
	if err != nil {
		t.Fatal(err)
	}
	if node.RevokedAt == nil {
		t.Fatal("expected revoked_at")
	}
	revoked, err = s.IsCertRevoked("fp-new")
	if err != nil || !revoked {
		t.Fatalf("current fp should be revoked after RevokeNode: %v %v", revoked, err)
	}
	_, err = s.RotateNodeCert("n-rot", CertMeta{Fingerprint: "fp-x", Serial: "sx"})
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict on revoked node, got %v", err)
	}
}

func TestMemoryStoreOwnerAndActorSub(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := newMemoryStoreWithNodes(t)

	// Lab: empty owner_sub OK
	sbEmpty, err := s.CreateSandbox(CreateSandboxInput{
		TenantID:  "t-owner",
		ImageRef:  "img",
		CPUMillis: 100,
		MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sbEmpty.OwnerSub != "" || sbEmpty.OwnerEmail != "" {
		t.Fatalf("expected empty owner fields, got sub=%q email=%q", sbEmpty.OwnerSub, sbEmpty.OwnerEmail)
	}
	evEmpty, err := s.ListEvents(sbEmpty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evEmpty) < 1 || evEmpty[0].ActorSub != "" {
		t.Fatalf("expected empty actor_sub on create, got %+v", evEmpty)
	}

	// Persist owner + actor_sub (body actor falls back when only owner set)
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID:   "t-owner",
		ImageRef:   "img",
		CPUMillis:  100,
		MemoryMiB:  128,
		OwnerSub:   "user:alice",
		OwnerEmail: "alice@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.OwnerSub != "user:alice" || sb.OwnerEmail != "alice@example.com" {
		t.Fatalf("owner fields: %+v", sb)
	}
	got, err := s.GetSandbox(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerSub != "user:alice" || got.OwnerEmail != "alice@example.com" {
		t.Fatalf("get owner fields: %+v", got)
	}
	list, err := s.ListSandboxes("t-owner")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		if x.ID == sb.ID {
			found = true
			if x.OwnerSub != "user:alice" {
				t.Fatalf("list owner_sub=%q", x.OwnerSub)
			}
		}
	}
	if !found {
		t.Fatal("sandbox missing from list")
	}
	ev, err := s.ListEvents(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) < 1 || ev[0].ActorSub != "user:alice" {
		t.Fatalf("create event actor_sub want user:alice, got %+v", ev)
	}

	// Explicit ActorSub wins over owner fallback
	sb2, err := s.CreateSandbox(CreateSandboxInput{
		TenantID:  "t-owner",
		ImageRef:  "img",
		CPUMillis: 100,
		MemoryMiB: 128,
		OwnerSub:  "user:bob",
		ActorSub:  "user:carol",
	})
	if err != nil {
		t.Fatal(err)
	}
	ev2, _ := s.ListEvents(sb2.ID)
	if len(ev2) < 1 || ev2[0].ActorSub != "user:carol" {
		t.Fatalf("explicit actor_sub: %+v", ev2)
	}

	// Destroy with actor_sub
	_, err = s.MarkSandboxStopping(sb2.ID, "user:carol")
	if err != nil {
		t.Fatal(err)
	}
	ev2, _ = s.ListEvents(sb2.ID)
	last := ev2[len(ev2)-1]
	if last.ActorSub != "user:carol" {
		t.Fatalf("destroy actor_sub=%q event=%+v", last.ActorSub, last)
	}
}

func TestDestroyFailedSandboxStops(t *testing.T) {
	s := newMemoryStoreWithNodes(t)
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID:  "tenant-a",
		ImageRef:  "img",
		CPUMillis: 100,
		MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(sb.ID, SandboxFailed, "virtiofs"); err != nil {
		t.Fatal(err)
	}
	out, err := s.MarkSandboxStopping(sb.ID, "user")
	if err != nil {
		t.Fatal(err)
	}
	if out.State != SandboxStopped {
		t.Fatalf("state %s", out.State)
	}
}

func TestRevokedNodeStaysRevokedUntilReEnroll(t *testing.T) {
	s := NewMemoryStore()
	if _, err := s.RegisterNode(RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeNode("n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HeartbeatNode("n1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("heartbeat after revoke: want ErrConflict, got %v", err)
	}
	if _, err := s.RegisterNode(RegisterNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("register after revoke: want ErrConflict, got %v", err)
	}
	n, err := s.GetNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.State != "offline" || n.RevokedAt == nil {
		t.Fatalf("revoked node came back: %+v", n)
	}

	// A fresh enroll is the way back.
	if _, err := s.EnrollNode(EnrollNodeInput{ID: "n1", AgentEndpoint: "http://127.0.0.1:9100"}, CertMeta{Fingerprint: "fp-new"}, EnrollAuth{}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.HeartbeatNode("n1"); err != nil || n.State != "ready" {
		t.Fatalf("heartbeat after re-enroll: node=%+v err=%v", n, err)
	}
}
