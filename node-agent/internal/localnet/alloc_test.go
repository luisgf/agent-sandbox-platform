package localnet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProbe reports tables and ports another program holds.
type fakeProbe struct{ tables, ports map[int]bool }

func (p fakeProbe) TablesInUse() map[int]bool  { return p.tables }
func (p fakeProbe) UDPPortsInUse() map[int]bool { return p.ports }

func TestAllocatorGivesCollidingShortIDsDistinctResources(t *testing.T) {
	dir := t.TempDir()
	a := NewAllocator(dir, nil)
	// Two ids whose hashed allocations collide: force it with ids that share
	// nothing but a precomputed table by allocating the first, then asking
	// for an id whose hash starts at the same table, port and slot.
	first := "aaaaaaaa-0000-0000-0000-000000000001"
	al1, err := a.Ensure(first)
	if err != nil {
		t.Fatal(err)
	}
	if al1 != HashAllocation(first) {
		t.Fatalf("a free hashed allocation is kept: %+v vs %+v", al1, HashAllocation(first))
	}
	// Occupy what the second id would hash to.
	second := "bbbbbbbb-0000-0000-0000-000000000002"
	h2 := HashAllocation(second)
	a.mu.Lock()
	a.tables[h2.TableID], a.ports[h2.ListenPort], a.slots[h2.Slot] = "other", "other", "other"
	a.mu.Unlock()
	al2, err := a.Ensure(second)
	if err != nil {
		t.Fatal(err)
	}
	if al2.TableID == h2.TableID || al2.ListenPort == h2.ListenPort || al2.Slot == h2.Slot {
		t.Fatalf("collision not avoided: %+v (hash %+v)", al2, h2)
	}
	if al2.TableID == al1.TableID || al2.ListenPort == al1.ListenPort || al2.Slot == al1.Slot || al2.NodeCIDR() == al1.NodeCIDR() {
		t.Fatalf("two sessions share resources: %+v and %+v", al1, al2)
	}
	if !al2.Valid() {
		t.Fatalf("out of range: %+v", al2)
	}
	if got, _ := a.Ensure(second); got != al2 {
		t.Fatal("Ensure must return the existing allocation")
	}
}

// Two sandboxes with the same short id get distinct tables, ports and /30s;
// the device names stay with the first, and the host refuses the second's
// plan instead of taking over the first's tunnel.
func TestSameShortIDGetsDistinctResourcesButNotTheDevices(t *testing.T) {
	dir := t.TempDir()
	h := NewHost(dir)
	h.Alloc = NewAllocator(dir, nil)
	first, second := "abcdef01-1111-1111-1111-111111111111", "abcdef01-2222-2222-2222-222222222222"
	a1, err := h.Allocation(first)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := h.Allocation(second)
	if err != nil {
		t.Fatal(err)
	}
	if a1.TableID == a2.TableID || a1.ListenPort == a2.ListenPort || a1.Slot == a2.Slot {
		t.Fatalf("shared resources: %+v %+v", a1, a2)
	}
	err = h.Apply(DecideWith(second, "o", true, "pending", a2))
	if err == nil || !strings.Contains(err.Error(), "wg-asp-abcdef01") || !strings.Contains(err.Error(), first) {
		t.Fatalf("second sandbox with the same short id: want a device-name error, got %v", err)
	}
	if err := h.Alloc.Release(first); err != nil {
		t.Fatal(err)
	}
	if got := h.Alloc.ShortHolder("abcdef01"); got != second {
		t.Fatalf("after the first leaves, the names go to the second: %q", got)
	}
}

func TestAllocatorPersistsReleasesAndReuses(t *testing.T) {
	dir := t.TempDir()
	id := "cccccccc-0000-0000-0000-000000000003"
	a := NewAllocator(dir, nil)
	al, err := a.Ensure(id)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, id+".alloc"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("allocation file: %v %v", fi, err)
	}
	// A restarted agent finds it again.
	b := NewAllocator(dir, nil)
	if got, ok := b.Get(id); !ok || got != al {
		t.Fatalf("after restart: %+v ok=%v", got, ok)
	}
	if owner, ok := b.TableOwner(al.TableID); !ok || owner != id {
		t.Fatalf("table owner: %q %v", owner, ok)
	}
	if err := b.Release(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".alloc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release must remove the file: %v", err)
	}
	// Freed resources are handed out again.
	again, err := b.Ensure(id)
	if err != nil || again != al {
		t.Fatalf("reuse: %+v %v (was %+v)", again, err, al)
	}
}

// Something else on the host holds the hashed table and port: skip them.
func TestAllocatorSkipsTablesAndPortsInUse(t *testing.T) {
	id := "dddddddd-0000-0000-0000-000000000004"
	h := HashAllocation(id)
	a := NewAllocator(t.TempDir(), fakeProbe{tables: map[int]bool{h.TableID: true}, ports: map[int]bool{h.ListenPort: true}})
	al, err := a.Ensure(id)
	if err != nil {
		t.Fatal(err)
	}
	if al.TableID == h.TableID || al.ListenPort == h.ListenPort {
		t.Fatalf("allocated what the host uses: %+v", al)
	}
	if al.Slot != h.Slot {
		t.Fatalf("the slot was free: %+v", al)
	}
}

func TestSystemProbeParsesIPRuleAndSS(t *testing.T) {
	p := SystemProbe{Run: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "ip":
			return []byte("0:\tfrom all lookup local\n13853:\tfrom all iif asp-x lookup 13853\n32766:\tfrom all lookup main\n20000:\tfrom all lookup 20000 proto static\n"), nil
		case "ss":
			return []byte("UNCONN 0 0 0.0.0.0:51024 0.0.0.0:*\nUNCONN 0 0 [::]:47001 [::]:*\n"), nil
		}
		return nil, errors.New("unknown")
	}}
	if tables := p.TablesInUse(); !tables[13853] || !tables[20000] || len(tables) != 2 {
		t.Fatalf("tables: %v", tables)
	}
	if ports := p.UDPPortsInUse(); !ports[51024] || !ports[47001] || len(ports) != 2 {
		t.Fatalf("ports: %v", ports)
	}
	missing := SystemProbe{Run: func(string, ...string) ([]byte, error) { return nil, errors.New("not found") }}
	if len(missing.TablesInUse()) != 0 || len(missing.UDPPortsInUse()) != 0 {
		t.Fatal("missing tools report nothing in use")
	}
}

func TestDecideWithUsesTheAllocation(t *testing.T) {
	al := Allocation{TableID: 12345, ListenPort: 50000, Slot: 7}
	p := DecideWith("eeeeeeee-0000-0000-0000-000000000005", "o", true, "up", al)
	if p.TableID != 12345 || p.ListenPort != 50000 || p.NodeCIDR != "10.188.0.29/30" || p.ClientCIDR != "10.188.0.30/30" {
		t.Fatalf("plan: %+v", p)
	}
}
