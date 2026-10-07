package vmm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Process is a running external process (cloud-hypervisor).
type Process interface {
	Wait() error
	Kill() error
	Pid() int
}

// Runner starts an external process. Injectable for unit tests.
type Runner interface {
	Start(name string, args ...string) (Process, error)
}

// DefaultRunner uses os/exec.Command.
type DefaultRunner struct{}

func (DefaultRunner) Start(name string, args ...string) (Process, error) {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec %s: %w", name, err)
	}
	return &execProcess{cmd: cmd}, nil
}

type execProcess struct {
	cmd *exec.Cmd
}

func (p *execProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	// A process that already ended (it is reaped by Wait) is what Kill wanted.
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (p *execProcess) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
