package vmm

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// FakeVMM records Create/Boot/Delete calls without talking to Cloud Hypervisor.
// Start/Stop track multiple sandbox IDs for the reconciler dry-run path.
type FakeVMM struct {
	mu      sync.Mutex
	Calls   []string
	Configs []MicroVMConfig
	Running map[string]MicroVMConfig
	Logger  *slog.Logger
	onExit  func(id string, info ExitInfo)
}

func NewFakeVMM(logger *slog.Logger) *FakeVMM {
	if logger == nil {
		logger = slog.Default()
	}
	return &FakeVMM{Logger: logger, Running: make(map[string]MicroVMConfig)}
}

func (f *FakeVMM) Ping(context.Context) error {
	f.record("ping")
	return nil
}

func (f *FakeVMM) Info(context.Context) (map[string]any, error) {
	f.record("info")
	return map[string]any{"fake": true}, nil
}

func (f *FakeVMM) CreateVM(_ context.Context, config MicroVMConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "create")
	f.Configs = append(f.Configs, config)
	f.Logger.Info("FakeVMM CreateVM", "id", config.ID, "cpus", config.CPUs, "memory_mib", config.MemoryMiB)
	return nil
}

func (f *FakeVMM) Boot(context.Context) error {
	f.record("boot")
	f.Logger.Info("FakeVMM Boot")
	return nil
}

func (f *FakeVMM) Delete(context.Context) error {
	f.record("delete")
	f.Logger.Info("FakeVMM Delete")
	return nil
}

func (f *FakeVMM) Start(ctx context.Context, config MicroVMConfig) error {
	if config.ID == "" {
		return fmt.Errorf("MicroVMConfig.ID required")
	}
	if err := f.CreateVM(ctx, config); err != nil {
		return err
	}
	if err := f.Boot(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Running == nil {
		f.Running = make(map[string]MicroVMConfig)
	}
	f.Running[config.ID] = config
	f.Calls = append(f.Calls, "start:"+config.ID)
	return nil
}

func (f *FakeVMM) Stop(ctx context.Context, id string) error {
	f.mu.Lock()
	if f.Running != nil {
		delete(f.Running, id)
	}
	f.Calls = append(f.Calls, "stop:"+id)
	f.mu.Unlock()
	f.Logger.Info("FakeVMM Stop", "id", id)
	return f.Delete(ctx)
}

func (f *FakeVMM) Pause(context.Context, string) error {
	f.record("pause")
	return nil
}

// SetExitHandler implements ExitNotifier.
func (f *FakeVMM) SetExitHandler(fn func(id string, info ExitInfo)) {
	f.mu.Lock()
	f.onExit = fn
	f.mu.Unlock()
}

// Crash makes the VM of id end on its own, as a Cloud Hypervisor that was killed
// does: the handler is told, and the VM stays tracked (as running) until Stop.
// It reports whether id was running.
func (f *FakeVMM) Crash(id string, err error, lived time.Duration) bool {
	f.mu.Lock()
	_, ok := f.Running[id]
	fn := f.onExit
	f.mu.Unlock()
	if ok && fn != nil {
		fn(id, ExitInfo{Err: err, Lived: lived})
	}
	return ok
}

// RunningConfig returns the config passed to Start for id.
func (f *FakeVMM) RunningConfig(id string) (MicroVMConfig, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cfg, ok := f.Running[id]
	return cfg, ok
}

func (f *FakeVMM) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, op)
}

var _ MicroVM = (*FakeVMM)(nil)
var _ VMM = (*FakeVMM)(nil)
var _ ExitNotifier = (*FakeVMM)(nil)
