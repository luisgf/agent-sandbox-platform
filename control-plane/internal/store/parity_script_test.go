package store

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// paritySteps is the script both stores play (see parity_test.go). It goes through
// every Store method on the way a deployment uses it, failures included: the unknown
// id, the repeated call, the call that conflicts. What the two stores answer to
// those is the contract, and it is where they have drifted apart.
func paritySteps() []step {
	var s []step
	add := func(method, name string, run func(w *world) (any, error)) {
		s = append(s, step{method: method, name: name, run: run})
	}
	addUnordered := func(method, name string, run func(w *world) (any, error)) {
		s = append(s, step{method: method, name: name, unordered: true, run: run})
	}
	// prepare steps change state through Store calls the script checks elsewhere.
	prepare := func(name string, run func(w *world) (any, error)) {
		s = append(s, step{name: name, run: run})
	}

	f := false
	t := true
	port443 := 443
	badPort := 70000

	// ---- nodes ----

	add("RegisterNode", "n1, every field", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{
			ID: "n1", Name: "node one", Endpoint: "10.0.0.1:9100", AgentEndpoint: "http://127.0.0.1:9101",
			VMMProfiles: []string{"cloud-hypervisor"}, CapacityCPU: 8, CapacityMemMiB: 16384, MaxSandboxes: 4,
			LocalNetDial: "n1.example:51820", EgressEnforced: true, AgentInstanceID: "inst-1", AgentVersion: "0.1.0",
			GuestKernelDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GuestImageDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		})
	})
	add("RegisterNode", "n2, only an id and an endpoint", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "n2", AgentEndpoint: "http://127.0.0.1:9102"})
	})
	add("RegisterNode", "n3, an agent without --reconcile", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "n3", AgentEndpoint: "http://127.0.0.1:9103", AcceptsWork: &f})
	})
	add("RegisterNode", "n6, room for one small sandbox", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "n6", AgentEndpoint: "http://127.0.0.1:9106", CapacityCPU: 1, CapacityMemMiB: 512, MaxSandboxes: 1})
	})
	add("RegisterNode", "no id and no name", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{AgentEndpoint: "http://127.0.0.1:9199"})
	})
	add("RegisterNode", "negative capacity", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "bad", CapacityCPU: -1})
	})
	add("RegisterNode", "n1 again with other values", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{
			ID: "n1", Name: "node one", Endpoint: "10.0.0.1:9100", AgentEndpoint: "http://127.0.0.1:9101",
			CapacityCPU: 6, CapacityMemMiB: 8192, LocalNetDial: "n1b.example:51820", AgentInstanceID: "inst-1", AgentVersion: "0.2.0",
			GuestKernelDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		})
	})
	add("RegisterNode", "a name another node has", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "n9", Name: "node one", AgentEndpoint: "http://127.0.0.1:9109"})
	})
	add("RegisterNode", "n2 takes the name of n1", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "n2", Name: "node one", AgentEndpoint: "http://127.0.0.1:9102"})
	})
	add("GetNode", "n1", func(w *world) (any, error) { return w.s.GetNode(bg, "n1") })
	add("GetNode", "unknown", func(w *world) (any, error) { return w.s.GetNode(bg, "nope") })
	addUnordered("ListNodes", "all", func(w *world) (any, error) { return w.s.ListNodes(bg) })
	add("HeartbeatNode", "n1", func(w *world) (any, error) { return w.s.HeartbeatNode(bg, "n1") })
	add("HeartbeatNode", "unknown", func(w *world) (any, error) { return w.s.HeartbeatNode(bg, "nope") })
	add("SetNodeDiskFree", "n1", func(w *world) (any, error) { return nil, w.s.SetNodeDiskFree(bg, "n1", 5000) })
	add("SetNodeDiskFree", "unknown", func(w *world) (any, error) { return nil, w.s.SetNodeDiskFree(bg, "nope", 5000) })
	add("TouchNodePoll", "n1", func(w *world) (any, error) { return nil, w.s.TouchNodePoll(bg, "n1", time.Now()) })
	add("TouchNodePoll", "unknown", func(w *world) (any, error) { return nil, w.s.TouchNodePoll(bg, "nope", time.Now()) })
	add("SetNodeCordoned", "cordon n2", func(w *world) (any, error) { return w.s.SetNodeCordoned(bg, "n2", true) })
	add("SetNodeCordoned", "unknown", func(w *world) (any, error) { return w.s.SetNodeCordoned(bg, "nope", true) })
	add("SetNodeFence", "set on n1", func(w *world) (any, error) { return w.s.SetNodeFence(bg, "n1", "http://fence.example/off", "token-1") })
	add("SetNodeFence", "unknown", func(w *world) (any, error) { return w.s.SetNodeFence(bg, "nope", "http://fence.example/off", "t") })
	add("SetNodeFence", "clear on n1", func(w *world) (any, error) { return w.s.SetNodeFence(bg, "n1", "", "") })
	add("GetNode", "n1 and n2 after the admin calls", func(w *world) (any, error) {
		a, err := w.s.GetNode(bg, "n1")
		if err != nil {
			return nil, err
		}
		b, err := w.s.GetNode(bg, "n2")
		return []Node{a, b}, err
	})
	add("EmitNodeEvent", "on n1", func(w *world) (any, error) {
		return nil, w.s.EmitNodeEvent(bg, "n1", "node.test", "tester", map[string]any{"k": "v"})
	})
	add("EmitNodeEvent", "on an unknown node", func(w *world) (any, error) {
		return nil, w.s.EmitNodeEvent(bg, "nope", "node.test", "tester", nil)
	})
	prepare("uncordon n2", func(w *world) (any, error) { return w.s.SetNodeCordoned(bg, "n2", false) })

	// ---- enrollment and certificates ----

	hash1, hash2, hash3, hashOld := HashEnrollToken("tok-1"), HashEnrollToken("tok-2"), HashEnrollToken("tok-3"), HashEnrollToken("tok-old")
	add("CreateEnrollToken", "pinned to n4", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{Hash: hash1, NodeID: "n4", ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "admin"})
	})
	add("CreateEnrollToken", "not pinned", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{Hash: hash2, ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "admin"})
	})
	add("CreateEnrollToken", "pinned to n4 for the re-enrollment", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{Hash: hash3, NodeID: "n4", ExpiresAt: time.Now().Add(time.Hour)})
	})
	add("CreateEnrollToken", "already expired", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{Hash: hashOld, ExpiresAt: time.Now().Add(-time.Hour)})
	})
	add("CreateEnrollToken", "no hash", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{ExpiresAt: time.Now().Add(time.Hour)})
	})
	add("CreateEnrollToken", "the same hash twice", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{Hash: hash1, NodeID: "n7", ExpiresAt: time.Now().Add(time.Hour)})
	})
	add("CheckEnroll", "n4 with its token", func(w *world) (any, error) { return nil, w.s.CheckEnroll(bg, "n4", EnrollAuth{TokenHash: hash1}) })
	add("CheckEnroll", "n5 with n4's token", func(w *world) (any, error) { return nil, w.s.CheckEnroll(bg, "n5", EnrollAuth{TokenHash: hash1}) })
	add("CheckEnroll", "an unknown token", func(w *world) (any, error) { return nil, w.s.CheckEnroll(bg, "n4", EnrollAuth{TokenHash: "nope"}) })
	add("CheckEnroll", "an expired token", func(w *world) (any, error) { return nil, w.s.CheckEnroll(bg, "n5", EnrollAuth{TokenHash: hashOld}) })
	add("CheckEnroll", "the bootstrap token, a new node", func(w *world) (any, error) { return nil, w.s.CheckEnroll(bg, "n4", EnrollAuth{}) })
	add("CheckEnroll", "the bootstrap token, a node with a live certificate", func(w *world) (any, error) {
		// n1 registered without a certificate; n2 likewise. Both may enroll.
		return nil, w.s.CheckEnroll(bg, "n1", EnrollAuth{})
	})
	cert := func(fp string) CertMeta {
		return CertMeta{Fingerprint: fp, Serial: "serial-" + fp, NotAfter: time.Now().Add(24 * time.Hour)}
	}
	add("EnrollNode", "n4 with its token", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n4", Name: "node four", AgentEndpoint: "http://127.0.0.1:9104", CapacityCPU: 2, CapacityMemMiB: 2048}, cert("fp-n4-a"), EnrollAuth{TokenHash: hash1})
	})
	add("EnrollNode", "n4 with the token again", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n4", AgentEndpoint: "http://127.0.0.1:9104"}, cert("fp-n4-x"), EnrollAuth{TokenHash: hash1})
	})
	add("EnrollNode", "n4, which has a live certificate, with the bootstrap token", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n4", AgentEndpoint: "http://127.0.0.1:9104"}, cert("fp-n4-y"), EnrollAuth{})
	})
	add("EnrollNode", "n5 with an unpinned token", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n5", AgentEndpoint: "http://127.0.0.1:9105"}, cert("fp-n5-a"), EnrollAuth{TokenHash: hash2})
	})
	hash4 := HashEnrollToken("tok-4")
	add("CreateEnrollToken", "for a node whose name is taken", func(w *world) (any, error) {
		return nil, w.s.CreateEnrollToken(bg, EnrollToken{Hash: hash4, ExpiresAt: time.Now().Add(time.Hour)})
	})
	add("EnrollNode", "a name another node has", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n8", Name: "node one", AgentEndpoint: "http://127.0.0.1:9108"}, cert("fp-n8"), EnrollAuth{TokenHash: hash4})
	})
	add("EnrollNode", "the token that failed on the name is still good", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n8", Name: "node eight", AgentEndpoint: "http://127.0.0.1:9108"}, cert("fp-n8"), EnrollAuth{TokenHash: hash4})
	})
	add("EnrollNode", "no node id", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{AgentEndpoint: "http://127.0.0.1:9100"}, cert("fp-none"), EnrollAuth{})
	})
	add("IsCertRevoked", "a live certificate", func(w *world) (any, error) { return w.s.IsCertRevoked(bg, "fp-n4-a") })
	add("IsCertRevoked", "an unknown fingerprint", func(w *world) (any, error) { return w.s.IsCertRevoked(bg, "fp-unknown") })
	add("RotateNodeCert", "n4", func(w *world) (any, error) { return w.s.RotateNodeCert(bg, "n4", cert("fp-n4-b")) })
	add("RotateNodeCert", "unknown", func(w *world) (any, error) { return w.s.RotateNodeCert(bg, "nope", cert("fp-x")) })
	add("IsCertRevoked", "the certificate n4 replaced", func(w *world) (any, error) { return w.s.IsCertRevoked(bg, "fp-n4-a") })
	add("RevokeNode", "n4", func(w *world) (any, error) { return w.s.RevokeNode(bg, "n4") })
	add("RevokeNode", "n4 again", func(w *world) (any, error) { return w.s.RevokeNode(bg, "n4") })
	add("RevokeNode", "unknown", func(w *world) (any, error) { return w.s.RevokeNode(bg, "nope") })
	add("IsCertRevoked", "the certificate of a revoked node", func(w *world) (any, error) { return w.s.IsCertRevoked(bg, "fp-n4-b") })
	add("HeartbeatNode", "revoked n4", func(w *world) (any, error) { return w.s.HeartbeatNode(bg, "n4") })
	add("RegisterNode", "revoked n4", func(w *world) (any, error) {
		return w.s.RegisterNode(bg, RegisterNodeInput{ID: "n4", AgentEndpoint: "http://127.0.0.1:9104"})
	})
	add("EnrollNode", "n4 re-enrolls with a token pinned to it", func(w *world) (any, error) {
		return w.s.EnrollNode(bg, EnrollNodeInput{ID: "n4", AgentEndpoint: "http://127.0.0.1:9104"}, cert("fp-n4-c"), EnrollAuth{TokenHash: hash3})
	})
	add("HeartbeatNode", "n4 after re-enrolling", func(w *world) (any, error) { return w.s.HeartbeatNode(bg, "n4") })

	// ---- sandboxes ----

	create := func(name string, in CreateSandboxInput) {
		add("CreateSandbox", name, func(w *world) (any, error) {
			sb, err := w.s.CreateSandbox(bg, in)
			if err == nil {
				w.bind(name, sb.ID)
			}
			// Lists are newest first: two creations in one clock tick would be
			// ordered by their ids, which differ between the stores.
			time.Sleep(2 * time.Millisecond)
			return sb, err
		})
	}
	// Bound names are the step names; keep the ones later steps use short.
	create("sb1", CreateSandboxInput{TenantID: "t1", ImageRef: "debian:bookworm", CPUMillis: 500, MemoryMiB: 256, NodeID: "n1",
		OwnerSub: "alice", OwnerEmail: "alice@example.com", WorkspaceHostPath: "/srv/ws/t1"})
	create("sb2", CreateSandboxInput{TenantID: "t1", ImageRef: "debian:bookworm", CPUMillis: 250, MemoryMiB: 128, NodeID: "n2", LocalNet: &t})
	create("sb3", CreateSandboxInput{TenantID: "t2", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n1"})
	create("sb4", CreateSandboxInput{TenantID: "t2", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64})
	add("CreateSandbox", "no image", func(w *world) (any, error) {
		return w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t1", CPUMillis: 100, MemoryMiB: 64})
	})
	add("CreateSandbox", "no tenant", func(w *world) (any, error) {
		return w.s.CreateSandbox(bg, CreateSandboxInput{ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64})
	})
	add("CreateSandbox", "pinned to an unknown node", func(w *world) (any, error) {
		return w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t1", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "nope"})
	})
	add("CreateSandbox", "pinned to a node that takes no work", func(w *world) (any, error) {
		return w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t1", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n3"})
	})
	add("CreateSandbox", "pinned to a revoked node", func(w *world) (any, error) {
		if _, err := w.s.RevokeNode(bg, "n5"); err != nil {
			return nil, err
		}
		return w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t1", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n5"})
	})
	create("sb5", CreateSandboxInput{TenantID: "t3", ImageRef: "alpine", CPUMillis: 500, MemoryMiB: 256, NodeID: "n6"})
	add("CreateSandbox", "a node that is full", func(w *world) (any, error) {
		return w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t3", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n6"})
	})
	add("CreateSandbox", "bigger than any node", func(w *world) (any, error) {
		return w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t3", ImageRef: "alpine", CPUMillis: 1, MemoryMiB: 1 << 20, NodeID: "n6"})
	})
	add("GetSandbox", "sb1", func(w *world) (any, error) { return w.s.GetSandbox(bg, w.id("sb1")) })
	add("GetSandbox", "unknown", func(w *world) (any, error) { return w.s.GetSandbox(bg, "nope") })
	add("ListSandboxes", "t1", func(w *world) (any, error) { return w.s.ListSandboxes(bg, "t1") })
	add("ListSandboxes", "every tenant", func(w *world) (any, error) { return w.s.ListSandboxes(bg, "") })
	add("ListSandboxes", "a tenant with none", func(w *world) (any, error) { return w.s.ListSandboxes(bg, "nobody") })
	add("ClaimSandbox", "sb1 by its node", func(w *world) (any, error) { return w.s.ClaimSandbox(bg, w.id("sb1"), "n1") })
	add("ClaimSandbox", "sb1 again", func(w *world) (any, error) { return w.s.ClaimSandbox(bg, w.id("sb1"), "n1") })
	add("ClaimSandbox", "sb2 by a node it is not placed on", func(w *world) (any, error) { return w.s.ClaimSandbox(bg, w.id("sb2"), "n1") })
	add("ClaimSandbox", "unknown", func(w *world) (any, error) { return w.s.ClaimSandbox(bg, "nope", "n1") })
	add("ListNodeWork", "n1", func(w *world) (any, error) { return w.s.ListNodeWork(bg, "n1") })
	add("ListNodeWork", "n2", func(w *world) (any, error) { return w.s.ListNodeWork(bg, "n2") })
	add("ListNodeWork", "an unknown node", func(w *world) (any, error) { return w.s.ListNodeWork(bg, "nope") })
	add("UpdateSandboxStatus", "sb1 running", func(w *world) (any, error) {
		return w.s.UpdateSandboxStatus(bg, w.id("sb1"), SandboxRunning, "")
	})
	add("UpdateSandboxStatus", "a state that does not exist", func(w *world) (any, error) {
		return w.s.UpdateSandboxStatus(bg, w.id("sb1"), SandboxState("bogus"), "")
	})
	add("UpdateSandboxStatus", "unknown", func(w *world) (any, error) {
		return w.s.UpdateSandboxStatus(bg, "nope", SandboxRunning, "")
	})
	add("TouchSandboxActivity", "sb1", func(w *world) (any, error) { return nil, w.s.TouchSandboxActivity(bg, w.id("sb1")) })
	add("TouchSandboxActivity", "unknown", func(w *world) (any, error) { return nil, w.s.TouchSandboxActivity(bg, "nope") })
	add("StopSandbox", "sb1, which runs", func(w *world) (any, error) { return w.s.StopSandbox(bg, w.id("sb1"), "bob") })
	add("StopSandbox", "sb1 again", func(w *world) (any, error) { return w.s.StopSandbox(bg, w.id("sb1"), "bob") })
	add("StopSandbox", "unknown", func(w *world) (any, error) { return w.s.StopSandbox(bg, "nope", "bob") })
	add("UpdateSandboxStatus", "sb1 stopped", func(w *world) (any, error) {
		return w.s.UpdateSandboxStatus(bg, w.id("sb1"), SandboxStopped, "")
	})
	add("CountStoppedByNode", "one stopped on n1", func(w *world) (any, error) { return w.s.CountStoppedByNode(bg) })
	add("ListNodeWork", "n1 with a stopped sandbox", func(w *world) (any, error) { return w.s.ListNodeWork(bg, "n1") })
	add("StopSandbox", "sb3, which no node claimed", func(w *world) (any, error) { return w.s.StopSandbox(bg, w.id("sb3"), "") })
	add("ResumeSandbox", "sb1", func(w *world) (any, error) { return w.s.ResumeSandbox(bg, w.id("sb1"), "carol") })
	add("ResumeSandbox", "sb1 again", func(w *world) (any, error) { return w.s.ResumeSandbox(bg, w.id("sb1"), "carol") })
	add("ResumeSandbox", "unknown", func(w *world) (any, error) { return w.s.ResumeSandbox(bg, "nope", "carol") })
	add("ResumeSandbox", "sb3, stopped before it ever ran", func(w *world) (any, error) { return w.s.ResumeSandbox(bg, w.id("sb3"), "") })
	add("DeleteSandbox", "sb2, which no node claimed", func(w *world) (any, error) { return w.s.DeleteSandbox(bg, w.id("sb2"), "dave") })
	add("DeleteSandbox", "sb2 again", func(w *world) (any, error) { return w.s.DeleteSandbox(bg, w.id("sb2"), "dave") })
	add("DeleteSandbox", "unknown", func(w *world) (any, error) { return w.s.DeleteSandbox(bg, "nope", "dave") })
	add("StopSandbox", "sb2, which is deleted", func(w *world) (any, error) { return w.s.StopSandbox(bg, w.id("sb2"), "") })
	add("ResumeSandbox", "sb2, which is deleted", func(w *world) (any, error) { return w.s.ResumeSandbox(bg, w.id("sb2"), "") })
	add("UpdateSandboxStatus", "sb4 failed", func(w *world) (any, error) {
		return w.s.UpdateSandboxStatus(bg, w.id("sb4"), SandboxFailed, "boom")
	})
	add("StopSandbox", "sb4, which failed", func(w *world) (any, error) { return w.s.StopSandbox(bg, w.id("sb4"), "") })
	add("ResumeSandbox", "sb4, which failed", func(w *world) (any, error) { return w.s.ResumeSandbox(bg, w.id("sb4"), "") })
	add("DeleteSandbox", "sb4, which failed", func(w *world) (any, error) { return w.s.DeleteSandbox(bg, w.id("sb4"), "") })
	add("GetSandbox", "sb1 after resume", func(w *world) (any, error) { return w.s.GetSandbox(bg, w.id("sb1")) })

	// ---- idle, retention, counts, usage ----

	running := func(name string, in CreateSandboxInput) {
		prepare(name+" running", func(w *world) (any, error) {
			sb, err := w.s.CreateSandbox(bg, in)
			if err != nil {
				return nil, err
			}
			w.bind(name, sb.ID)
			if _, err := w.s.ClaimSandbox(bg, sb.ID, in.NodeID); err != nil {
				return nil, err
			}
			return w.s.UpdateSandboxStatus(bg, sb.ID, SandboxRunning, "")
		})
	}
	running("r1", CreateSandboxInput{TenantID: "t4", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n2"})
	running("r2", CreateSandboxInput{TenantID: "t4", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n2"})
	running("r3", CreateSandboxInput{TenantID: "t5", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n2"})
	add("CountSandboxes", "per tenant and state", func(w *world) (any, error) {
		got, err := w.s.CountSandboxes(bg)
		return got, err
	})
	add("ListNodeUsage", "all nodes", func(w *world) (any, error) { return w.s.ListNodeUsage(bg) })
	addUnordered("StopIdleSandboxes", "nothing is idle yet", func(w *world) (any, error) {
		return w.s.StopIdleSandboxes(bg, time.Now(), time.Hour)
	})
	addUnordered("StopIdleSandboxes", "disabled", func(w *world) (any, error) {
		return w.s.StopIdleSandboxes(bg, time.Now().Add(3*time.Hour), 0)
	})
	prepare("r2 is used", func(w *world) (any, error) { return nil, w.s.TouchSandboxActivity(bg, w.id("r2")) })
	addUnordered("StopIdleSandboxes", "everything active is idle three hours on", func(w *world) (any, error) {
		return w.s.StopIdleSandboxes(bg, time.Now().Add(3*time.Hour), time.Hour)
	})
	prepare("n2 powers off the idle ones", func(w *world) (any, error) {
		var out []Sandbox
		for _, n := range []string{"r1", "r2", "r3"} {
			sb, err := w.s.UpdateSandboxStatus(bg, w.id(n), SandboxStopped, "")
			if err != nil {
				return nil, err
			}
			out = append(out, sb)
			// "Oldest" is by stop time: two stops in one clock tick would make the
			// order the ids', which differ between the stores.
			time.Sleep(2 * time.Millisecond)
		}
		return out, nil
	})
	addUnordered("ExpireStoppedSandboxes", "nothing stopped long enough", func(w *world) (any, error) {
		return w.s.ExpireStoppedSandboxes(bg, time.Now(), 24*time.Hour)
	})
	addUnordered("ExpireStoppedSandboxes", "disabled", func(w *world) (any, error) {
		return w.s.ExpireStoppedSandboxes(bg, time.Now().Add(48*time.Hour), 0)
	})
	addUnordered("EvictStoppedOverCap", "disabled", func(w *world) (any, error) { return w.s.EvictStoppedOverCap(bg, 0) })
	addUnordered("EvictStoppedOverCap", "one stopped per tenant", func(w *world) (any, error) { return w.s.EvictStoppedOverCap(bg, 1) })
	addUnordered("ExpireStoppedSandboxes", "all the rest, two days on", func(w *world) (any, error) {
		return w.s.ExpireStoppedSandboxes(bg, time.Now().Add(48*time.Hour), 24*time.Hour)
	})
	add("CountSandboxes", "after retention", func(w *world) (any, error) { return w.s.CountSandboxes(bg) })
	add("CountStoppedByNode", "after retention", func(w *world) (any, error) { return w.s.CountStoppedByNode(bg) })
	add("ListNodeUsage", "after retention", func(w *world) (any, error) { return w.s.ListNodeUsage(bg) })

	// ---- a lost node ----

	running("l1", CreateSandboxInput{TenantID: "t6", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n6"})
	prepare("l2 requested on n1", func(w *world) (any, error) {
		sb, err := w.s.CreateSandbox(bg, CreateSandboxInput{TenantID: "t6", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n1"})
		if err == nil {
			w.bind("l2", sb.ID)
		}
		return sb, err
	})
	add("MarkNodeOffline", "n1, seen after silentSince", func(w *world) (any, error) {
		return w.s.MarkNodeOffline(bg, "n1", time.Now().Add(-time.Hour))
	})
	add("FailNodeSandboxes", "n1, seen after silentSince", func(w *world) (any, error) {
		return w.s.FailNodeSandboxes(bg, "n1", "node_lost", time.Now().Add(-time.Hour))
	})
	add("MarkNodeOffline", "n1, silent", func(w *world) (any, error) {
		return w.s.MarkNodeOffline(bg, "n1", time.Now().Add(time.Hour))
	})
	add("MarkNodeOffline", "n1 again", func(w *world) (any, error) {
		return w.s.MarkNodeOffline(bg, "n1", time.Now().Add(time.Hour))
	})
	add("MarkNodeOffline", "a revoked node", func(w *world) (any, error) {
		return w.s.MarkNodeOffline(bg, "n5", time.Now().Add(time.Hour))
	})
	add("MarkNodeOffline", "unknown", func(w *world) (any, error) {
		return w.s.MarkNodeOffline(bg, "nope", time.Now().Add(time.Hour))
	})
	addUnordered("FailNodeSandboxes", "n1, silent", func(w *world) (any, error) {
		return w.s.FailNodeSandboxes(bg, "n1", "node_lost", time.Now().Add(time.Hour))
	})
	addUnordered("FailNodeSandboxes", "n1 again", func(w *world) (any, error) {
		return w.s.FailNodeSandboxes(bg, "n1", "node_lost", time.Now().Add(time.Hour))
	})
	addUnordered("FailNodeSandboxes", "unknown", func(w *world) (any, error) {
		return w.s.FailNodeSandboxes(bg, "nope", "node_lost", time.Now().Add(time.Hour))
	})
	addUnordered("FailUnassignedRequested", "none are unassigned", func(w *world) (any, error) {
		return w.s.FailUnassignedRequested(bg, time.Now().Add(time.Hour), "never_placed")
	})
	add("ListNodes", "n1 is offline now", func(w *world) (any, error) { return w.s.GetNode(bg, "n1") })
	add("HeartbeatNode", "n1 comes back", func(w *world) (any, error) { return w.s.HeartbeatNode(bg, "n1") })

	// ---- events ----

	add("EmitEvent", "custom event on sb1", func(w *world) (any, error) {
		from, to := "running", "stopping"
		req := "req-1"
		return nil, w.s.EmitEvent(bg, EmitEventInput{
			SandboxID: w.id("sb1"), TenantID: "t1", EventType: "custom.test", FromState: &from, ToState: &to,
			Actor: "tester", ActorSub: "alice", RequestID: &req, Payload: json.RawMessage(`{"b":2,"a":{"x":[1,2]}}`),
		})
	})
	add("EmitEvent", "no payload", func(w *world) (any, error) {
		return nil, w.s.EmitEvent(bg, EmitEventInput{SandboxID: w.id("sb1"), TenantID: "t1", EventType: "custom.bare", Actor: "tester"})
	})
	add("EmitEvent", "an unknown sandbox", func(w *world) (any, error) {
		return nil, w.s.EmitEvent(bg, EmitEventInput{SandboxID: "nope", TenantID: "t1", EventType: "custom.test", Actor: "tester"})
	})
	add("EmitEvent", "no event type", func(w *world) (any, error) {
		return nil, w.s.EmitEvent(bg, EmitEventInput{SandboxID: w.id("sb1"), TenantID: "t1", Actor: "tester"})
	})
	add("ListEvents", "sb1: every transition so far", func(w *world) (any, error) { return w.s.ListEvents(bg, w.id("sb1")) })
	add("ListEvents", "sb2: created and deleted", func(w *world) (any, error) { return w.s.ListEvents(bg, w.id("sb2")) })
	add("ListEvents", "sb4: failed", func(w *world) (any, error) { return w.s.ListEvents(bg, w.id("sb4")) })
	add("ListEvents", "l1: lost with its node", func(w *world) (any, error) { return w.s.ListEvents(bg, w.id("l1")) })
	add("ListEvents", "r2: idle, stopped, expired", func(w *world) (any, error) { return w.s.ListEvents(bg, w.id("r2")) })
	add("ListEvents", "an unknown sandbox", func(w *world) (any, error) { return w.s.ListEvents(bg, "nope") })

	// ---- api keys ----

	hash := func(secret string) string { return HashAPIKeySecret(secret) }
	exp := func(h time.Duration) *time.Time { t := time.Now().Add(h); return &t }
	add("EnsureAPIKey", "the bootstrap key", func(w *world) (any, error) {
		k, err := w.s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0001", hash("boot0001-secret"))
		if err == nil {
			w.bind("kboot", k.ID)
		}
		return k, err
	})
	add("EnsureAPIKey", "the same key again", func(w *world) (any, error) {
		return w.s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0001", hash("boot0001-secret"))
	})
	add("EnsureAPIKey", "no secret", func(w *world) (any, error) {
		return w.s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0001", "")
	})
	add("EnsureAPIKey", "a scope that does not exist", func(w *world) (any, error) {
		return w.s.EnsureAPIKey(bg, "default", "other", "galactic", "othr0001", hash("other"))
	})
	add("CreateAPIKey", "ci key with an expiry", func(w *world) (any, error) {
		k, err := w.s.CreateAPIKey(bg, "t1", "ci", APIKeyScopeTenant, "ci000001", hash("ci000001-secret"), exp(time.Hour))
		if err == nil {
			w.bind("kci", k.ID)
		}
		return k, err
	})
	add("CreateAPIKey", "a name the tenant already has", func(w *world) (any, error) {
		return w.s.CreateAPIKey(bg, "t1", "ci", APIKeyScopeTenant, "ci000002", hash("ci000002-secret"), nil)
	})
	add("CreateAPIKey", "a prefix another key has", func(w *world) (any, error) {
		return w.s.CreateAPIKey(bg, "t2", "ci", APIKeyScopeTenant, "ci000001", hash("ci000003-secret"), nil)
	})
	add("CreateAPIKey", "a secret another key has", func(w *world) (any, error) {
		return w.s.CreateAPIKey(bg, "t2", "dup-secret", APIKeyScopeTenant, "ci000004", hash("ci000001-secret"), nil)
	})
	add("CreateAPIKey", "no prefix", func(w *world) (any, error) {
		return w.s.CreateAPIKey(bg, "t2", "noprefix", APIKeyScopeTenant, "", hash("x"), nil)
	})
	add("CreateAPIKey", "a scope that does not exist", func(w *world) (any, error) {
		return w.s.CreateAPIKey(bg, "t2", "scope", "galactic", "scope001", hash("scope"), nil)
	})
	add("CreateAPIKey", "an already expired key", func(w *world) (any, error) {
		k, err := w.s.CreateAPIKey(bg, "t2", "old", APIKeyScopeTenant, "old00001", hash("old00001-secret"), exp(-time.Hour))
		if err == nil {
			w.bind("kold", k.ID)
		}
		return k, err
	})
	add("CreateAPIKey", "a key of a tenant that has no sandboxes", func(w *world) (any, error) {
		k, err := w.s.CreateAPIKey(bg, "fresh", "k", APIKeyScopeTenant, "fresh001", hash("fresh001-secret"), nil)
		if err == nil {
			w.bind("kfresh", k.ID)
		}
		return k, err
	})
	add("EnsureAPIKey", "a prefix another key has", func(w *world) (any, error) {
		return w.s.EnsureAPIKey(bg, "t9", "ensured", APIKeyScopeTenant, "ci000001", hash("ensured-secret"))
	})
	add("EnsureAPIKey", "a secret another key has", func(w *world) (any, error) {
		return w.s.EnsureAPIKey(bg, "t9", "ensured2", APIKeyScopeTenant, "ensr0002", hash("ci000001-secret"))
	})
	add("LookupAPIKeyByHash", "the ci key", func(w *world) (any, error) { return w.s.LookupAPIKeyByHash(bg, hash("ci000001-secret")) })
	add("LookupAPIKeyByHash", "an expired key", func(w *world) (any, error) { return w.s.LookupAPIKeyByHash(bg, hash("old00001-secret")) })
	add("LookupAPIKeyByHash", "an unknown secret", func(w *world) (any, error) { return w.s.LookupAPIKeyByHash(bg, hash("nope")) })
	add("TouchAPIKey", "the ci key", func(w *world) (any, error) { return nil, w.s.TouchAPIKey(bg, w.id("kci")) })
	add("TouchAPIKey", "unknown", func(w *world) (any, error) { return nil, w.s.TouchAPIKey(bg, "nope") })
	add("GetAPIKey", "the ci key", func(w *world) (any, error) { return w.s.GetAPIKey(bg, w.id("kci")) })
	add("GetAPIKey", "unknown", func(w *world) (any, error) { return w.s.GetAPIKey(bg, "nope") })
	add("ListAPIKeys", "t1", func(w *world) (any, error) { return w.s.ListAPIKeys(bg, "t1") })
	add("ListAPIKeys", "every tenant", func(w *world) (any, error) { return w.s.ListAPIKeys(bg, "") })
	add("ListAPIKeys", "a tenant with none", func(w *world) (any, error) { return w.s.ListAPIKeys(bg, "nobody") })
	add("CountAPIKeys", "all live", func(w *world) (any, error) { return w.s.CountAPIKeys(bg) })
	add("RotateAPIKey", "the ci key", func(w *world) (any, error) {
		return w.s.RotateAPIKey(bg, w.id("kci"), "ci000009", hash("ci000009-secret"))
	})
	add("LookupAPIKeyByHash", "the secret the rotation replaced", func(w *world) (any, error) {
		return w.s.LookupAPIKeyByHash(bg, hash("ci000001-secret"))
	})
	add("RotateAPIKey", "to a prefix another key has", func(w *world) (any, error) {
		return w.s.RotateAPIKey(bg, w.id("kci"), "boot0001", hash("rot-clash"))
	})
	add("RotateAPIKey", "to a secret another key has", func(w *world) (any, error) {
		return w.s.RotateAPIKey(bg, w.id("kci"), "ci000010", hash("boot0001-secret"))
	})
	add("RotateAPIKey", "unknown", func(w *world) (any, error) { return w.s.RotateAPIKey(bg, "nope", "nope0001", hash("n")) })
	add("RotateAPIKey", "no secret", func(w *world) (any, error) { return w.s.RotateAPIKey(bg, w.id("kci"), "ci000011", "") })
	add("RevokeAPIKey", "the ci key", func(w *world) (any, error) { return w.s.RevokeAPIKey(bg, w.id("kci")) })
	add("RevokeAPIKey", "the ci key again", func(w *world) (any, error) { return w.s.RevokeAPIKey(bg, w.id("kci")) })
	add("RevokeAPIKey", "unknown", func(w *world) (any, error) { return w.s.RevokeAPIKey(bg, "nope") })
	add("RotateAPIKey", "a revoked key", func(w *world) (any, error) {
		return w.s.RotateAPIKey(bg, w.id("kci"), "ci000012", hash("ci000012-secret"))
	})
	add("LookupAPIKeyByHash", "a revoked key", func(w *world) (any, error) { return w.s.LookupAPIKeyByHash(bg, hash("ci000009-secret")) })
	add("CountAPIKeys", "after a revocation", func(w *world) (any, error) { return w.s.CountAPIKeys(bg) })
	add("EnsureAPIKey", "revoke the bootstrap key, then ensure it with the same secret", func(w *world) (any, error) {
		if _, err := w.s.RevokeAPIKey(bg, w.id("kboot")); err != nil {
			return nil, err
		}
		return w.s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0001", hash("boot0001-secret"))
	})
	add("EnsureAPIKey", "the revoked bootstrap key with a new secret", func(w *world) (any, error) {
		return w.s.EnsureAPIKey(bg, "default", "bootstrap", APIKeyScopePlatform, "boot0002", hash("boot0002-secret"))
	})
	add("LookupAPIKeyByHash", "the bootstrap key's old secret", func(w *world) (any, error) {
		return w.s.LookupAPIKeyByHash(bg, hash("boot0001-secret"))
	})
	add("LookupAPIKeyByHash", "the bootstrap key's new secret", func(w *world) (any, error) {
		return w.s.LookupAPIKeyByHash(bg, hash("boot0002-secret"))
	})
	add("ListAPIKeys", "every tenant at the end", func(w *world) (any, error) { return w.s.ListAPIKeys(bg, "") })

	// ---- egress rules ----

	add("ListEgressRules", "a tenant with none", func(w *world) (any, error) { return w.s.ListEgressRules(bg, "t1") })
	add("PutEgressRules", "three rules", func(w *world) (any, error) {
		return w.s.PutEgressRules(bg, "t1", []EgressRule{
			{HostPattern: "*.github.com", Enabled: true},
			{HostPattern: "registry.npmjs.org", Port: &port443, Enabled: true},
			{HostPattern: "disabled.example", Enabled: false},
		})
	})
	add("ListEgressRules", "t1", func(w *world) (any, error) { return w.s.ListEgressRules(bg, "t1") })
	add("PutEgressRules", "a different tenant", func(w *world) (any, error) {
		return w.s.PutEgressRules(bg, "t2", []EgressRule{{HostPattern: "example.org", Enabled: true}})
	})
	add("ListEgressRulesForTenants", "t1, t2 and one with none", func(w *world) (any, error) {
		return w.s.ListEgressRulesForTenants(bg, []string{"t1", "t2", "nobody"})
	})
	add("ListEgressRulesForTenants", "no tenants", func(w *world) (any, error) { return w.s.ListEgressRulesForTenants(bg, nil) })
	add("PutEgressRules", "no host", func(w *world) (any, error) {
		return w.s.PutEgressRules(bg, "t1", []EgressRule{{HostPattern: "", Enabled: true}})
	})
	add("PutEgressRules", "a port out of range", func(w *world) (any, error) {
		return w.s.PutEgressRules(bg, "t1", []EgressRule{{HostPattern: "example.com", Port: &badPort, Enabled: true}})
	})
	add("PutEgressRules", "the same rule twice", func(w *world) (any, error) {
		return w.s.PutEgressRules(bg, "t3", []EgressRule{{HostPattern: "dup.example", Enabled: true}, {HostPattern: "dup.example", Enabled: true}})
	})
	add("PutEgressRules", "no tenant", func(w *world) (any, error) {
		return w.s.PutEgressRules(bg, "", []EgressRule{{HostPattern: "example.com", Enabled: true}})
	})
	add("ListEgressRules", "t1 still has its three", func(w *world) (any, error) { return w.s.ListEgressRules(bg, "t1") })
	add("PutEgressRules", "an empty set clears", func(w *world) (any, error) { return w.s.PutEgressRules(bg, "t1", nil) })
	add("ListEgressRules", "t1 after clearing", func(w *world) (any, error) { return w.s.ListEgressRules(bg, "t1") })

	// ---- attestation ----

	statementTS := time.Date(2026, 10, 7, 12, 0, 0, 123456000, time.UTC)
	attest := func(w *world, sandbox, digest string) (any, error) {
		rec, err := w.s.PutAttestation(bg, PutAttestationInput{
			SandboxID: w.id(sandbox), NodeID: "n1", ImageDigest: digest, VMMProfile: "cloud-hypervisor", CID: 7,
			StatementTS: statementTS, Alg: "ed25519", KeyID: "k1", Signature: "sig", Bundle: json.RawMessage(`{"measured":{"kernel":"abc"}}`),
		})
		return map[string]any{"record": rec, "statement_ts": rec.StatementTS.UTC().Format(time.RFC3339Nano)}, err
	}
	add("PutAttestation", "sb1", func(w *world) (any, error) { return attest(w, "sb1", "sha256:one") })
	add("PutAttestation", "sb1 again replaces it", func(w *world) (any, error) { return attest(w, "sb1", "sha256:two") })
	add("PutAttestation", "an unknown sandbox", func(w *world) (any, error) { return attest(w, "nope", "sha256:x") })
	add("GetAttestation", "sb1", func(w *world) (any, error) {
		rec, err := w.s.GetAttestation(bg, w.id("sb1"))
		return map[string]any{"record": rec, "statement_ts": rec.StatementTS.UTC().Format(time.RFC3339Nano)}, err
	})
	add("GetAttestation", "a sandbox with none", func(w *world) (any, error) { return w.s.GetAttestation(bg, w.id("sb3")) })
	add("GetAttestation", "unknown", func(w *world) (any, error) { return w.s.GetAttestation(bg, "nope") })

	// ---- local net ----

	pub := base64.StdEncoding.EncodeToString(make([]byte, 32))
	pub2 := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	running("ln", CreateSandboxInput{TenantID: "t7", ImageRef: "alpine", CPUMillis: 100, MemoryMiB: 64, NodeID: "n2", LocalNet: &t})
	grant := func(w *world, ttl time.Duration, now time.Time) (any, error) {
		g, exp, err := w.s.IssueLocalNetGrant(bg, w.id("ln"), "n2.example:51820", now, ttl)
		w.memo["grant"] = g
		return map[string]any{"grant_length": len(g), "ttl": exp.Sub(now).String()}, err
	}
	add("IssueLocalNetGrant", "a sandbox that did not opt in", func(w *world) (any, error) {
		g, _, err := w.s.IssueLocalNetGrant(bg, w.id("sb1"), "n2.example:51820", time.Now(), time.Minute)
		return len(g), err
	})
	add("IssueLocalNetGrant", "unknown", func(w *world) (any, error) {
		g, _, err := w.s.IssueLocalNetGrant(bg, "nope", "n2.example:51820", time.Now(), time.Minute)
		return len(g), err
	})
	add("IssueLocalNetGrant", "ln", func(w *world) (any, error) { return grant(w, 10*time.Minute, time.Now()) })
	add("HeartbeatLocalNet", "a wrong grant", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, w.id("ln"), "wrong", pub, time.Now())
	})
	add("HeartbeatLocalNet", "a bad public key", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, w.id("ln"), w.memo["grant"], "not-a-key", time.Now())
	})
	add("HeartbeatLocalNet", "the grant", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, w.id("ln"), w.memo["grant"], pub, time.Now())
	})
	add("HeartbeatLocalNet", "the grant again", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, w.id("ln"), w.memo["grant"], pub, time.Now())
	})
	add("HeartbeatLocalNet", "a sandbox that did not opt in", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, w.id("sb1"), "x", pub, time.Now())
	})
	add("HeartbeatLocalNet", "unknown", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, "nope", "x", pub, time.Now())
	})
	add("SetLocalNetNodePublic", "the node's key and tunnel", func(w *world) (any, error) {
		return w.s.SetLocalNetNodePublic(bg, w.id("ln"), pub2, LocalNetTunnel{ListenPort: 51820, NodeAddr: "10.188.4.1/30", ClientAddr: "10.188.4.2/30"})
	})
	add("SetLocalNetNodePublic", "a tunnel outside the range", func(w *world) (any, error) {
		return w.s.SetLocalNetNodePublic(bg, w.id("ln"), pub2, LocalNetTunnel{ListenPort: 51820, NodeAddr: "192.168.0.1/30", ClientAddr: "192.168.0.2/30"})
	})
	add("SetLocalNetNodePublic", "a bad key", func(w *world) (any, error) {
		return w.s.SetLocalNetNodePublic(bg, w.id("ln"), "not-a-key", LocalNetTunnel{ListenPort: 51820, NodeAddr: "10.188.4.1/30", ClientAddr: "10.188.4.2/30"})
	})
	add("SetLocalNetNodePublic", "unknown", func(w *world) (any, error) {
		return w.s.SetLocalNetNodePublic(bg, "nope", pub2, LocalNetTunnel{ListenPort: 51820, NodeAddr: "10.188.4.1/30", ClientAddr: "10.188.4.2/30"})
	})
	add("WithdrawLocalNet", "ln", func(w *world) (any, error) { return w.s.WithdrawLocalNet(bg, w.id("ln")) })
	add("WithdrawLocalNet", "a sandbox that did not opt in", func(w *world) (any, error) { return w.s.WithdrawLocalNet(bg, w.id("sb1")) })
	add("WithdrawLocalNet", "unknown", func(w *world) (any, error) { return w.s.WithdrawLocalNet(bg, "nope") })
	add("IssueLocalNetGrant", "ln, a grant that is already over", func(w *world) (any, error) {
		return grant(w, time.Minute, time.Now().Add(-time.Hour))
	})
	add("HeartbeatLocalNet", "the expired grant", func(w *world) (any, error) {
		return w.s.HeartbeatLocalNet(bg, w.id("ln"), w.memo["grant"], pub, time.Now())
	})
	add("GetSandbox", "ln after an expired grant", func(w *world) (any, error) { return w.s.GetSandbox(bg, w.id("ln")) })
	add("ListEvents", "ln", func(w *world) (any, error) { return w.s.ListEvents(bg, w.id("ln")) })

	return s
}
