package sshagent

import (
	"net"
	"testing"
	"time"
)

func TestSignRequestDeniedWithoutApproval(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ap := NewApprover(time.Second)
	go ConfirmingServeConn(c2, "", ap, nil)

	if _, err := c1.Write(SignRequestRaw()); err != nil {
		t.Fatal(err)
	}
	msgType, err := ReadAgentReply(c1)
	if err != nil {
		t.Fatal(err)
	}
	if msgType != sshAgentFailure {
		t.Fatalf("want SSH_AGENT_FAILURE (%d), got %d", sshAgentFailure, msgType)
	}
}

func TestSignRequestAllowedAfterApprove(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ap := NewApprover(time.Minute)
	tok, _ := ap.Approve(time.Minute)
	if tok == "" || ap.PendingCount() != 1 {
		t.Fatalf("approve failed tok=%q pending=%d", tok, ap.PendingCount())
	}
	go ConfirmingServeConn(c2, "", ap, nil)

	if _, err := c1.Write(SignRequestRaw()); err != nil {
		t.Fatal(err)
	}
	msgType, err := ReadAgentReply(c1)
	if err != nil {
		t.Fatal(err)
	}
	// No upstream agent → FakeAgent path returns FAILURE even when approved,
	// but approval must have been consumed (not blocked earlier as unapproved).
	if msgType != sshAgentFailure {
		t.Fatalf("want failure from FakeAgent after consume, got %d", msgType)
	}
	if ap.PendingCount() != 0 {
		t.Fatalf("approval should be consumed, pending=%d", ap.PendingCount())
	}

	// Second sign without new approval → denied path (still failure, pending stays 0)
	if _, err := c1.Write(SignRequestRaw()); err != nil {
		t.Fatal(err)
	}
	msgType, err = ReadAgentReply(c1)
	if err != nil {
		t.Fatal(err)
	}
	if msgType != sshAgentFailure {
		t.Fatalf("want failure, got %d", msgType)
	}
}

func TestIdentitiesStillWorkWithConfirmGate(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ap := NewApprover(time.Second)
	go ConfirmingServeConn(c2, "", ap, nil)
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
	ap.Approve(5 * time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if ap.Consume() {
		t.Fatal("expired approval should not consume")
	}
}
