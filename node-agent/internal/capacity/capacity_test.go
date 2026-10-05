package capacity

import (
	"strings"
	"testing"
)

func TestParseMemTotal(t *testing.T) {
	meminfo := "MemTotal:       16303368 kB\nMemFree:         1234567 kB\nMemAvailable:    9876543 kB\n"
	if got := parseMemTotal(strings.NewReader(meminfo)); got != 15921 {
		t.Fatalf("parseMemTotal = %d, want 15921", got)
	}
	if got := parseMemTotal(strings.NewReader("garbage\n")); got != 0 {
		t.Fatalf("no MemTotal: %d", got)
	}
}

func TestResolve(t *testing.T) {
	h := Host{CPUs: 16, MemTotalMiB: 65536}
	if got := CPU(Detected, h); got != 16 {
		t.Errorf("CPU detect = %d", got)
	}
	if got := CPU(0, h); got != 0 {
		t.Errorf("CPU not enforced = %d", got)
	}
	if got := CPU(8, h); got != 8 {
		t.Errorf("CPU explicit = %d", got)
	}
	if got := MemMiB(Detected, h); got != 65536-6553 {
		t.Errorf("MemMiB 10%% reserve = %d", got)
	}
	if got := MemMiB(Detected, Host{MemTotalMiB: 4096}); got != 3072 {
		t.Errorf("MemMiB 1 GiB reserve = %d", got)
	}
	if got := MemMiB(Detected, Host{MemTotalMiB: 900}); got != 1 {
		t.Errorf("MemMiB tiny host = %d, want 1", got)
	}
	if got := MemMiB(Detected, Host{}); got != 0 {
		t.Errorf("MemMiB unknown = %d, want 0 (not enforced)", got)
	}
	if got := MemMiB(2048, h); got != 2048 {
		t.Errorf("MemMiB explicit = %d", got)
	}
}
