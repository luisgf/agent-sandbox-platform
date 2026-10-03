package store

import (
	"errors"
	"testing"
	"time"
)

func TestDecideIdle(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		last    time.Time
		timeout time.Duration
		want    IdleVerdict
	}{
		{name: "disabled zero", last: now.Add(-3 * time.Hour), timeout: 0, want: IdleDisabled},
		{name: "disabled negative", last: now.Add(-3 * time.Hour), timeout: -time.Hour, want: IdleDisabled},
		{name: "active inside 2h", last: now.Add(-90 * time.Minute), timeout: RecommendedIdleTimeout, want: IdleActive},
		{name: "active inside 1h", last: now.Add(-30 * time.Minute), timeout: time.Hour, want: IdleActive},
		{name: "expired past 2h", last: now.Add(-2*time.Hour - time.Second), timeout: RecommendedIdleTimeout, want: IdleExpired},
		{name: "expired past 1h", last: now.Add(-61 * time.Minute), timeout: time.Hour, want: IdleExpired},
		{name: "expired at exact threshold", last: now.Add(-2 * time.Hour), timeout: RecommendedIdleTimeout, want: IdleExpired},
		{name: "zero clock is not expired", last: time.Time{}, timeout: time.Hour, want: IdleActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideIdle(tc.last, now, tc.timeout)
			if got != tc.want {
				t.Fatalf("DecideIdle=%s want %s", got, tc.want)
			}
		})
	}
}

func TestParseIdleTimeout(t *testing.T) {
	for _, raw := range []string{"", "0", "0s", "off", "OFF", "false", "disabled", "no", "none"} {
		d, err := ParseIdleTimeout(raw)
		if err != nil || d != 0 {
			t.Fatalf("raw %q → %s err=%v, want disabled", raw, d, err)
		}
	}
	d, err := ParseIdleTimeout("2h")
	if err != nil || d != 2*time.Hour {
		t.Fatalf("2h → %s %v", d, err)
	}
	d, err = ParseIdleTimeout("1h")
	if err != nil || d != time.Hour {
		t.Fatalf("1h → %s %v", d, err)
	}
	d, err = ParseIdleTimeout("90m")
	if err != nil || d != 90*time.Minute {
		t.Fatalf("90m → %s %v", d, err)
	}
	if _, err := ParseIdleTimeout("tomorrow"); err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("garbage err=%v", err)
	}
	flagWins, err := ResolveIdleTimeout("2h", "off")
	if err != nil || flagWins != 0 {
		t.Fatalf("flag override → %s %v", flagWins, err)
	}
	envOnly, err := ResolveIdleTimeout("1h", "  ")
	if err != nil || envOnly != time.Hour {
		t.Fatalf("env only → %s %v", envOnly, err)
	}
	if IdleSweepInterval("") != time.Minute {
		t.Fatal("default sweep")
	}
	if IdleSweepInterval("15s") != 15*time.Second {
		t.Fatal("sweep 15s")
	}
	if IdleSweepInterval("nope") != time.Minute {
		t.Fatal("bad sweep falls back")
	}
}

func TestStopIdleSandboxesMemory(t *testing.T) {
	s := NewMemoryStore()
	fresh, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128, NodeID: "n1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastActivityAt.IsZero() {
		t.Fatal("create must stamp last_activity_at")
	}
	stale, err := s.CreateSandbox(CreateSandboxInput{
		TenantID: "t", ImageRef: "img", CPUMillis: 100, MemoryMiB: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pin a node so the assigned one goes to stopping, not straight to stopped.
	if _, err := s.UpdateSandboxStatus(fresh.ID, SandboxRunning, ""); err != nil {
		t.Fatal(err)
	}
	running, err := s.GetSandbox(fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.State != SandboxRunning || running.StopReason != "" {
		t.Fatalf("after start: %+v", running)
	}
	if !running.LastActivityAt.After(fresh.LastActivityAt.Add(-time.Nanosecond)) && running.LastActivityAt.Before(fresh.LastActivityAt) {
		t.Fatalf("running should refresh activity: create=%s running=%s", fresh.LastActivityAt, running.LastActivityAt)
	}

	past := time.Now().UTC().Add(-3 * time.Hour)
	s.SetLastActivityForTest(running.ID, past)
	s.SetLastActivityForTest(stale.ID, past)

	// Disabled reaper touches nothing.
	none, err := s.StopIdleSandboxes(time.Now().UTC(), 0)
	if err != nil || len(none) != 0 {
		t.Fatalf("disabled: %v %+v", err, none)
	}
	still, _ := s.GetSandbox(running.ID)
	if still.State != SandboxRunning {
		t.Fatalf("disabled changed state to %s", still.State)
	}

	// Recent exec keeps the running sandbox; the untouched one is expired.
	if err := s.TouchSandboxActivity(running.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	reaped, err := s.StopIdleSandboxes(now, RecommendedIdleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].ID != stale.ID {
		t.Fatalf("reaped=%+v", reaped)
	}
	if reaped[0].State != SandboxStopped || reaped[0].StopReason != StopReasonIdle {
		t.Fatalf("unassigned requested should stop immediately: %+v", reaped[0])
	}
	kept, err := s.GetSandbox(running.ID)
	if err != nil || kept.State != SandboxRunning || kept.StopReason != "" {
		t.Fatalf("active sandbox: %+v %v", kept, err)
	}

	// After the touch ages out, the running sandbox is marked stopping for the node reconciler.
	s.SetLastActivityForTest(running.ID, past)
	reaped, err = s.StopIdleSandboxes(time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].ID != running.ID {
		t.Fatalf("second reap=%+v", reaped)
	}
	if reaped[0].State != SandboxStopping || reaped[0].StopReason != StopReasonIdle {
		t.Fatalf("running idle: %+v", reaped[0])
	}
	ev, err := s.ListEvents(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ev {
		if e.EventType == "sandbox.idle_reaped" && e.Actor == "idle-reaper" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing idle_reaped event: %+v", ev)
	}

	// Already stopping is not reaped again.
	again, err := s.StopIdleSandboxes(time.Now().UTC().Add(5*time.Hour), time.Hour)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass: %v %+v", err, again)
	}
}
