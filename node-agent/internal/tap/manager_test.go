package tap

import (
	"errors"
	"strings"
	"testing"
)

func TestDeviceName(t *testing.T) {
	if got := DeviceName("abcdef12-zzzz"); got != "asp-abcdef12" {
		t.Fatalf("got %s", got)
	}
	if got := DeviceName("short"); got != "asp-short" {
		t.Fatalf("got %s", got)
	}
}

func TestCreateDeleteRecorded(t *testing.T) {
	rec := &RecordingRunner{}
	m := &Manager{Runner: rec, HostCIDR: "10.9.0.1/24"}
	if err := m.Create("asp-test0001"); err != nil {
		t.Fatal(err)
	}
	if len(rec.Calls) != 3 {
		t.Fatalf("calls=%v", rec.Calls)
	}
	if !strings.Contains(rec.Calls[0], "tuntap add") {
		t.Fatalf("first=%s", rec.Calls[0])
	}
	if !strings.Contains(rec.Calls[2], "10.9.0.1/24") {
		t.Fatalf("addr=%s", rec.Calls[2])
	}
	rec.Calls = nil
	if err := m.Delete("asp-test0001"); err != nil {
		t.Fatal(err)
	}
	if len(rec.Calls) != 1 || !strings.Contains(rec.Calls[0], "link delete") {
		t.Fatalf("delete calls=%v", rec.Calls)
	}
}

func TestSoftFail(t *testing.T) {
	rec := &RecordingRunner{InjectedErr: errors.New("Operation not permitted")}
	m := &Manager{Runner: rec, SoftFail: true}
	if err := m.Create("asp-x"); err != nil {
		t.Fatal("soft fail should swallow:", err)
	}
}

func TestHardFail(t *testing.T) {
	rec := &RecordingRunner{InjectedErr: errors.New("Operation not permitted")}
	m := &Manager{Runner: rec, SoftFail: false}
	if err := m.Create("asp-x"); err == nil {
		t.Fatal("expected error")
	}
}
