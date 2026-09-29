package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
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
	_, err := s.CreateSandbox(CreateSandboxInput{TenantID: "", ImageRef: "x", CPUMillis: 1, MemoryMiB: 1})
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
	k, err := s.EnsureAPIKey("default", "bootstrap", KeyPrefix(secret), hash)
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
	k2, err := s.EnsureAPIKey("default", "bootstrap", KeyPrefix(secret), hash)
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
	}, CertMeta{Fingerprint: "abc123fingerprint", Serial: "aa"})
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
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 1, NodeID: "n-enroll",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.NodeID == nil || *sb.NodeID != "n-enroll" {
		t.Fatalf("node pin failed: %+v", sb)
	}
}

func TestMemoryStoreCreateRequestedWithoutAutoProvision(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.State != SandboxRequested {
		t.Fatalf("want requested, got %s", sb.State)
	}
	if sb.NodeID != nil {
		t.Fatalf("expected unassigned, got %v", sb.NodeID)
	}
}

func TestMemoryStoreSoftAssignOnCreate(t *testing.T) {
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
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	wins := make(chan string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			node := fmt.Sprintf("node-%d", i)
			got, err := s.ClaimSandbox(sb.ID, node)
			if err == nil {
				wins <- *got.NodeID
			}
		}(i)
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
	if got.State != SandboxStarting {
		t.Fatalf("state=%s", got.State)
	}
	if got.NodeID == nil || *got.NodeID != winners[0] {
		t.Fatalf("node=%v winner=%s", got.NodeID, winners[0])
	}

	// Second claim by other node conflicts
	_, err = s.ClaimSandbox(sb.ID, "other")
	if err == nil {
		t.Fatal("expected conflict")
	}
}

func TestMemoryStoreDestroyAndStatus(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 1, NodeID: "n1",
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
	stopping, err := s.MarkSandboxStopping(sb.ID)
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
	for _, w := range work {
		if w.ID == sb.ID {
			t.Fatalf("stopped sandbox still in work: %+v", w)
		}
	}
}

func TestMemoryStoreLeaseRenewAndExpiryReclaim(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimSandbox(sb.ID, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.NodeLeaseUntil == nil {
		t.Fatal("expected lease after claim")
	}
	running, err := s.UpdateSandboxStatus(sb.ID, SandboxRunning, "up")
	if err != nil {
		t.Fatal(err)
	}
	if running.NodeLeaseUntil == nil {
		t.Fatal("expected lease after running")
	}

	renewed, err := s.RenewSandboxLease(sb.ID, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if renewed.NodeLeaseUntil == nil || !renewed.NodeLeaseUntil.After(time.Now().UTC()) {
		t.Fatalf("renew lease: %+v", renewed.NodeLeaseUntil)
	}
	if _, err := s.RenewSandboxLease(sb.ID, "node-b"); err == nil {
		t.Fatal("other node renew should conflict")
	}

	// Force expire
	s.mu.Lock()
	got := s.sandboxes[sb.ID]
	past := time.Now().UTC().Add(-time.Minute)
	got.NodeLeaseUntil = &past
	s.sandboxes[sb.ID] = got
	s.mu.Unlock()

	reclaimed, err := s.ReclaimExpiredLeases(time.Now().UTC(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].State != SandboxFailed {
		t.Fatalf("reclaim failed: %+v", reclaimed)
	}

	// Fresh sandbox: expire + re-request then other node claims
	sb2, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSandbox(sb2.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(sb2.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got = s.sandboxes[sb2.ID]
	got.NodeLeaseUntil = &past
	s.sandboxes[sb2.ID] = got
	s.mu.Unlock()

	reclaimed, err = s.ReclaimExpiredLeases(time.Now().UTC(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].State != SandboxRequested {
		t.Fatalf("re-request: %+v", reclaimed)
	}
	if reclaimed[0].NodeID != nil {
		t.Fatalf("expected cleared node, got %v", reclaimed[0].NodeID)
	}
	takeover, err := s.ClaimSandbox(sb2.ID, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if takeover.NodeID == nil || *takeover.NodeID != "node-b" {
		t.Fatalf("takeover=%+v", takeover)
	}
}

func TestMemoryStoreClaimReclaimsExpiredRunning(t *testing.T) {
	t.Setenv("ASP_AUTO_PROVISION", "0")
	s := NewMemoryStore()
	sb, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 1, MemoryMiB: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSandbox(sb.ID, "node-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandboxStatus(sb.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.sandboxes[sb.ID]
	past := time.Now().UTC().Add(-time.Hour)
	got.NodeLeaseUntil = &past
	s.sandboxes[sb.ID] = got
	s.mu.Unlock()

	takeover, err := s.ClaimSandbox(sb.ID, "node-b")
	if err != nil {
		t.Fatal(err)
	}
	if takeover.State != SandboxStarting || takeover.NodeID == nil || *takeover.NodeID != "node-b" {
		t.Fatalf("%+v", takeover)
	}
}

func TestMemoryStoreRotateAndRevokeCert(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.EnrollNode(EnrollNodeInput{
		ID: "n-rot", Name: "n-rot", AgentEndpoint: "http://127.0.0.1:9",
	}, CertMeta{Fingerprint: "fp-old", Serial: "s1"})
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
