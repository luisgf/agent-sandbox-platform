package sshagent

import (
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/sshagent/agenttest"
)

// signFrom sends one SIGN_REQUEST through a connection served for sandboxID
// and returns the reply type.
func signFrom(t *testing.T, up *agenttest.Upstream, sandboxID string, ap *Approver) byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c1.Close()
	go ServeConn(c2, up.Path, sandboxID, ap, nil)
	reply, err := agenttest.Request(c1, agenttest.SignRequestMsg())
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

func countSigns(up *agenttest.Upstream) int {
	n := 0
	for _, typ := range up.Seen() {
		if typ == agenttest.SignRequest {
			n++
		}
	}
	return n
}

func TestSignRequestDeniedWithoutApproval(t *testing.T) {
	up := agenttest.Start(t, filepath.Join(t.TempDir(), "up.sock"))
	if got := signFrom(t, up, "sb-a", NewApprover(time.Minute)); got != agenttest.Failure {
		t.Fatalf("sign without approval: got %d, want FAILURE", got)
	}
	if n := countSigns(up); n != 0 {
		t.Fatalf("a denied sign reached the upstream agent %d times", n)
	}
}

// An approval for sandbox A does not unlock a sign from sandbox B.
func TestApprovalIsScopedToItsSandbox(t *testing.T) {
	up := agenttest.Start(t, filepath.Join(t.TempDir(), "up.sock"))
	ap := NewApprover(time.Minute)
	if _, _, err := ap.Approve("sb-a", time.Minute); err != nil {
		t.Fatal(err)
	}

	if got := signFrom(t, up, "sb-b", ap); got != agenttest.Failure {
		t.Fatalf("sb-b signed with sb-a's approval: got %d", got)
	}
	if ap.PendingCount("sb-a") != 1 {
		t.Fatal("sb-b's attempt used sb-a's approval")
	}
	if got := signFrom(t, up, "sb-a", ap); got != agenttest.SignResponse {
		t.Fatalf("sb-a with its approval: got %d, want SIGN_RESPONSE", got)
	}
	if got := signFrom(t, up, "sb-a", ap); got != agenttest.Failure {
		t.Fatalf("the approval is one-shot: second sign got %d", got)
	}
	if n := countSigns(up); n != 1 {
		t.Fatalf("upstream got %d signs, want 1", n)
	}
}

// Listeners that cannot tell guests apart need global approvals, and only
// they consume them.
func TestGlobalApprovals(t *testing.T) {
	up := agenttest.Start(t, filepath.Join(t.TempDir(), "up.sock"))

	strict := NewApprover(time.Minute)
	if _, _, err := strict.Approve("", time.Minute); !errors.Is(err, ErrSandboxRequired) {
		t.Fatalf("approve without sandbox: err=%v, want ErrSandboxRequired", err)
	}
	if _, _, err := strict.Approve("sb-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := signFrom(t, up, "", strict); got != agenttest.Failure {
		t.Fatalf("a listener without a sandbox used sb-a's approval: got %d", got)
	}

	lab := NewApprover(time.Minute)
	lab.GlobalApprovals = true
	if _, _, err := lab.Approve("", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := signFrom(t, up, "sb-a", lab); got != agenttest.Failure {
		t.Fatalf("a sandbox's listener used a global approval: got %d", got)
	}
	if got := signFrom(t, up, "", lab); got != agenttest.SignResponse {
		t.Fatalf("global listener with a global approval: got %d, want SIGN_RESPONSE", got)
	}
}

func TestIdentitiesStillWorkWithConfirmGate(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	go ServeConn(c2, "", "sb-a", NewApprover(time.Second), nil)
	count, err := RequestIdentities(c1)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count=%d", count)
	}
}

func TestApproverExpires(t *testing.T) {
	ap := NewApprover(10 * time.Millisecond)
	if _, _, err := ap.Approve("sb-a", 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if ap.Consume("sb-a") {
		t.Fatal("expired approval should not consume")
	}
}

// A sign uses the approval that would expire first.
func TestConsumeTakesTheApprovalThatExpiresFirst(t *testing.T) {
	ap := NewApprover(time.Minute)
	if _, _, err := ap.Approve("sb-a", time.Hour); err != nil {
		t.Fatal(err)
	}
	short, _, err := ap.Approve("sb-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := ap.consume("sb-a")
	if !ok || id != short {
		t.Fatalf("consumed %q (ok=%v), want the one-minute approval %q", id, ok, short)
	}
	if ap.PendingCount("sb-a") != 1 {
		t.Fatal("the one-hour approval should still be pending")
	}
}
