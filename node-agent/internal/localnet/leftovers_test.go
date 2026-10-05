package localnet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestIfacesListsOnlyWireGuardDevices(t *testing.T) {
	sys := t.TempDir()
	for name, uevent := range map[string]string{
		"wg-asp-aaaa1111": "DEVTYPE=wireguard\nINTERFACE=wg-asp-aaaa1111\nIFINDEX=7\n",
		"wg-asp-bbbb2222": "INTERFACE=wg-asp-bbbb2222\nIFINDEX=8\n", // shares the prefix, not WireGuard
		"wg0":             "DEVTYPE=wireguard\nINTERFACE=wg0\n",
		"asp-aaaa1111":    "INTERFACE=asp-aaaa1111\n",
	} {
		dir := filepath.Join(sys, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "uevent"), []byte(uevent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Ifaces(sys)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"wg-asp-aaaa1111"}) {
		t.Fatalf("ifaces=%v", got)
	}
}

func TestKeyIDsListsNodeKeys(t *testing.T) {
	dir := t.TempDir()
	id := "abcdef01-2345-4678-9abc-def012345678"
	h := NewHost(dir)
	if _, _, _, err := h.EnsureNodeKey(id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "old.key"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := h.KeyIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{id}) {
		t.Fatalf("ids=%v want [%s]", got, id)
	}
}
