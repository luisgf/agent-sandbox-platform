package tap

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
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

func TestDevicesListsOnlyTapDevices(t *testing.T) {
	sys := t.TempDir()
	for name, files := range map[string][]string{
		"asp-aaaa1111":    {"tun_flags", "uevent"},
		"asp-bbbb2222":    {"uevent"}, // shares the prefix, not a TUN/TAP device
		"wg-asp-cccc3333": {"uevent"},
		"tap0":            {"tun_flags"},
		"eth0":            {"uevent"},
	} {
		dir := filepath.Join(sys, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("0x1002\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := Devices(sys)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"asp-aaaa1111"}) {
		t.Fatalf("devices=%v", got)
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

func TestSlot(t *testing.T) {
	sub := netip.MustParsePrefix("10.200.0.0/16")
	n0, err := Slot(sub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n0.HostCIDR() != "10.200.0.1/30" || n0.Guest.String() != "10.200.0.2" {
		t.Fatalf("slot 0 = %s guest %s", n0.HostCIDR(), n0.Guest)
	}
	n65, err := Slot(sub, 65)
	if err != nil {
		t.Fatal(err)
	}
	if n65.Prefix.String() != "10.200.1.4/30" || n65.HostCIDR() != "10.200.1.5/30" {
		t.Fatalf("slot 65 = %s host %s", n65.Prefix, n65.HostCIDR())
	}
	if n65.KernelIPArg() != "ip=10.200.1.6::10.200.1.5:255.255.255.252::eth0:off" {
		t.Fatalf("ip arg = %s", n65.KernelIPArg())
	}
	if n0.Prefix.Overlaps(n65.Prefix) {
		t.Fatal("slots overlap")
	}
	if _, err := Slot(sub, 1<<14); err == nil {
		t.Fatal("want exhaustion error past the last /30")
	}
	if _, err := Slot(netip.MustParsePrefix("10.0.0.0/30"), 0); err == nil {
		t.Fatal("want error for a pool smaller than /29")
	}
}

func TestCreateWithCIDR(t *testing.T) {
	rec := &RecordingRunner{}
	m := &Manager{Runner: rec}
	if err := m.CreateWithCIDR("asp-test0002", "10.200.0.5/30"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Calls[2], "addr add 10.200.0.5/30 dev asp-test0002") {
		t.Fatalf("addr=%s", rec.Calls[2])
	}
}
