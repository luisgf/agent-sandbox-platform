package localnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Allocation is what a local-net session holds on its node: a policy-routing
// table, the UDP port of its WireGuard device and a /30 in 10.188.0.0/16.
// They used to be hashes of the 8-character short id, which collide: two
// sessions sharing a table send one session's traffic into the other's tunnel.
type Allocation struct {
	TableID    int `json:"table_id"`
	ListenPort int `json:"listen_port"`
	Slot       int `json:"slot"` // /30 index in 10.188.0.0/16
}

const (
	tableMin, tableEnd = 10000, 30000 // [min, end): never main (254) or local (255)
	portMin, portEnd   = 47000, 55000
	slotCount          = 16384 // /30s in a /16
	allocSuffix        = ".alloc"
)

// NodeCIDR is the node device address.
func (a Allocation) NodeCIDR() string { return slotCIDR(a.Slot, 1) }

// ClientCIDR is the address the local agent puts on its device.
func (a Allocation) ClientCIDR() string { return slotCIDR(a.Slot, 2) }

func slotCIDR(slot, host int) string {
	base := slot * 4
	return fmt.Sprintf("10.188.%d.%d/30", (base>>8)&0xff, base&0xff+host)
}

// Valid reports whether every field is inside its range.
func (a Allocation) Valid() bool {
	return a.TableID >= tableMin && a.TableID < tableEnd &&
		a.ListenPort >= portMin && a.ListenPort < portEnd &&
		a.Slot >= 0 && a.Slot < slotCount
}

// HashAllocation is the allocation derived from the short id: where the
// allocator starts looking (so it usually matches what older nodes and
// control planes computed) and what an older node used.
func HashAllocation(id string) Allocation {
	return Allocation{TableID: TableID(id), ListenPort: ListenPort(id), Slot: tunnelSlot(id)}
}

// HostProbe reports tables and UDP ports the host already uses, so the
// allocator does not hand out something another program holds.
type HostProbe interface {
	TablesInUse() map[int]bool
	UDPPortsInUse() map[int]bool
}

// SystemProbe asks ip(8) and ss(8). Missing tools report nothing in use.
type SystemProbe struct {
	// Run runs a command and returns its output (tests inject one).
	Run func(name string, args ...string) ([]byte, error)
}

var (
	lookupRe = regexp.MustCompile(`\blookup (\d+)\b`)
	portRe   = regexp.MustCompile(`:(\d+)$`)
)

func (p SystemProbe) run(name string, args ...string) []byte {
	run := p.Run
	if run == nil {
		run = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() }
	}
	out, err := run(name, args...)
	if err != nil {
		return nil
	}
	return out
}

// TablesInUse lists the numeric tables that `ip rule show` routes to.
func (p SystemProbe) TablesInUse() map[int]bool {
	used := map[int]bool{}
	for _, m := range lookupRe.FindAllStringSubmatch(string(p.run("ip", "rule", "show")), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			used[n] = true
		}
	}
	return used
}

// UDPPortsInUse lists the UDP ports bound locally (`ss -lun`).
func (p SystemProbe) UDPPortsInUse() map[int]bool {
	used := map[int]bool{}
	for _, line := range strings.Split(string(p.run("ss", "-H", "-lun")), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// State Recv-Q Send-Q Local-Address:Port Peer-Address:Port
		if m := portRe.FindStringSubmatch(fields[3]); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				used[n] = true
			}
		}
	}
	return used
}

// Allocator hands out allocations and keeps them in Dir/<sandbox>.alloc, next
// to the node keys, so a restarted agent and the reaper find them again.
type Allocator struct {
	Dir   string
	Probe HostProbe // nil: no host check

	mu     sync.Mutex
	loaded bool
	byID   map[string]Allocation
	short  map[string]string // short id → the sandbox that owns its device names
	tables map[int]string
	ports  map[int]string
	slots  map[int]string
}

// NewAllocator keeps allocations in dir.
func NewAllocator(dir string, probe HostProbe) *Allocator {
	return &Allocator{Dir: dir, Probe: probe}
}

func (a *Allocator) path(id string) string {
	id = strings.ReplaceAll(strings.TrimSpace(id), string(os.PathSeparator), "_")
	return filepath.Join(a.Dir, id+allocSuffix)
}

// loadLocked reads every allocation file once. Unreadable files are skipped:
// their sandbox gets a fresh allocation.
func (a *Allocator) loadLocked() {
	if a.loaded {
		return
	}
	a.loaded = true
	a.byID = map[string]Allocation{}
	a.short = map[string]string{}
	a.tables, a.ports, a.slots = map[int]string{}, map[int]string{}, map[int]string{}
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), allocSuffix)
		if !ok || e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(a.Dir, e.Name()))
		if err != nil {
			continue
		}
		var al Allocation
		if json.Unmarshal(raw, &al) != nil || !al.Valid() {
			continue
		}
		a.recordLocked(id, al)
	}
}

func (a *Allocator) recordLocked(id string, al Allocation) {
	a.byID[id] = al
	if _, held := a.short[ShortID(id)]; !held {
		a.short[ShortID(id)] = id
	}
	a.tables[al.TableID] = id
	a.ports[al.ListenPort] = id
	a.slots[al.Slot] = id
}

// IDs lists the sandboxes that hold an allocation.
func (a *Allocator) IDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked()
	out := make([]string, 0, len(a.byID))
	for id := range a.byID {
		out = append(out, id)
	}
	return out
}

// Get returns the allocation of sandboxID, if it has one.
func (a *Allocator) Get(sandboxID string) (Allocation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked()
	al, ok := a.byID[sandboxID]
	return al, ok
}

// TableOwner returns the sandbox that holds table, if any.
func (a *Allocator) TableOwner(table int) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked()
	id, ok := a.tables[table]
	return id, ok
}

// ShortHolder returns the sandbox that owns the device names of short id
// (wg-asp-<short>, asp-<short>): the first one allocated with it.
func (a *Allocator) ShortHolder(short string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked()
	return a.short[short]
}

// Ensure returns the allocation of sandboxID, allocating and persisting one
// the first time; it fails only when a range is full. Sandboxes whose short
// ids collide get distinct tables, ports and /30s, but the device names stay
// with the first one (see ShortHolder).
func (a *Allocator) Ensure(sandboxID string) (Allocation, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return Allocation{}, errors.New("sandbox id required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked()
	if al, ok := a.byID[sandboxID]; ok {
		return al, nil
	}
	usedTables, usedPorts := map[int]bool{}, map[int]bool{}
	if a.Probe != nil {
		usedTables, usedPorts = a.Probe.TablesInUse(), a.Probe.UDPPortsInUse()
	}
	pref := HashAllocation(sandboxID)
	table, ok := pick(pref.TableID, tableMin, tableEnd, func(n int) bool { _, held := a.tables[n]; return !held && !usedTables[n] })
	if !ok {
		return Allocation{}, errors.New("local-net: no free routing table")
	}
	port, ok := pick(pref.ListenPort, portMin, portEnd, func(n int) bool { _, held := a.ports[n]; return !held && !usedPorts[n] })
	if !ok {
		return Allocation{}, errors.New("local-net: no free UDP port")
	}
	slot, ok := pick(pref.Slot, 0, slotCount, func(n int) bool { _, held := a.slots[n]; return !held })
	if !ok {
		return Allocation{}, errors.New("local-net: no free tunnel /30")
	}
	al := Allocation{TableID: table, ListenPort: port, Slot: slot}
	raw, _ := json.Marshal(al)
	if err := os.MkdirAll(a.Dir, 0o700); err != nil {
		return Allocation{}, err
	}
	if err := writeAtomic(a.path(sandboxID), raw, 0o600); err != nil {
		return Allocation{}, fmt.Errorf("persist local-net allocation: %w", err)
	}
	a.recordLocked(sandboxID, al)
	return al, nil
}

// Release frees the allocation of sandboxID and removes its file.
func (a *Allocator) Release(sandboxID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadLocked()
	if al, ok := a.byID[sandboxID]; ok {
		delete(a.byID, sandboxID)
		delete(a.tables, al.TableID)
		delete(a.ports, al.ListenPort)
		delete(a.slots, al.Slot)
		if a.short[ShortID(sandboxID)] == sandboxID {
			delete(a.short, ShortID(sandboxID))
			// The next sandbox with this short id, if any, owns the names now.
			for id := range a.byID {
				if ShortID(id) == ShortID(sandboxID) {
					a.short[ShortID(id)] = id
					break
				}
			}
		}
	}
	if err := os.Remove(a.path(sandboxID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// pick returns the first n in [min, end), starting at start and wrapping,
// for which free(n) holds.
func pick(start, min, end int, free func(int) bool) (int, bool) {
	size := end - min
	if start < min || start >= end {
		start = min
	}
	for i := 0; i < size; i++ {
		n := min + (start-min+i)%size
		if free(n) {
			return n, true
		}
	}
	return 0, false
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
