package localnet

import (
	"fmt"
	"sync"
)

// Applier records or installs the per-sandbox plan.
type Applier interface {
	Apply(p Plan) error
	Clear(sandboxID string) error
	Current(sandboxID string) (Plan, bool)
}

// Verifier is implemented by appliers that can cheaply tell whether an
// applied plan's devices still exist.
type Verifier interface {
	Healthy(sandboxID string) bool
}

// Memory is the CI/FakeVMM applier. It does not touch the host routing table.
type Memory struct {
	mu    sync.Mutex
	plans map[string]Plan
}

func NewMemory() *Memory {
	return &Memory{plans: map[string]Plan{}}
}

func (m *Memory) Apply(p Plan) error {
	if m == nil {
		return fmt.Errorf("localnet applier is nil")
	}
	if p.SandboxID == "" {
		return fmt.Errorf("sandbox id required")
	}
	if p.Kind != KindPublic && p.UsePublicProxy {
		return fmt.Errorf("refusing public proxy fallback for %s", p.SandboxID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plans == nil {
		m.plans = map[string]Plan{}
	}
	m.plans[p.SandboxID] = p
	return nil
}

func (m *Memory) Clear(sandboxID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.plans, sandboxID)
	return nil
}

func (m *Memory) Current(sandboxID string) (Plan, bool) {
	if m == nil {
		return Plan{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[sandboxID]
	return p, ok
}
