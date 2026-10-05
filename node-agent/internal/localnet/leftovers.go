package localnet

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Ifaces lists the WireGuard devices under sysClassNet (/sys/class/net) named
// like a plan's Iface. The reaper clears the ones a previous agent left.
func Ifaces(sysClassNet string) ([]string, error) {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, IfacePrefix) {
			continue
		}
		uevent, err := os.ReadFile(filepath.Join(sysClassNet, name, "uevent"))
		if err != nil || !slices.Contains(strings.Fields(string(uevent)), "DEVTYPE=wireguard") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// KeyIDs lists the sandboxes that have a node key in KeyDir. Every local-net
// sandbox gets one before its plan is applied and Clear removes it, so a key
// marks tunnel state (device, rules, table) that may still be installed, even
// for a blackholed session that has no device.
func (h *Host) KeyIDs() ([]string, error) {
	entries, err := os.ReadDir(h.KeyDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".key")
		if !ok || id == "" || !e.Type().IsRegular() {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
