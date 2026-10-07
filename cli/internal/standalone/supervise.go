package standalone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// proc is a program asp-server keeps running.
type proc struct {
	name string
	// cmd makes the command for one run; it is called again for each restart.
	cmd func() *exec.Cmd
	// grace is how long a stopping process has to finish before it is killed.
	grace time.Duration
	// refusal is the exit code of a process that refused its settings: starting it again
	// would refuse again, so asp-server stops instead.
	refusal int
	out     io.Writer
	log     *slog.Logger
	// backoff bounds the wait between a process dying and its restart.
	minBackoff, maxBackoff time.Duration
}

// errRefused is the error of a process that refused to start.
type errRefused struct {
	name string
	code int
}

func (e errRefused) Error() string {
	return fmt.Sprintf("%s refused its settings (exit %d) and was not restarted: its output is above", e.name, e.code)
}

// run keeps the process running until ctx ends, restarting it when it dies. It returns nil
// when ctx ended, errRefused when the process refused its settings, or the error of a
// process that cannot be started at all.
func (p *proc) run(ctx context.Context) error {
	backoff := p.minBackoff
	for {
		cmd := p.cmd()
		cmd.Stdout = &prefixWriter{prefix: "[" + p.name + "] ", w: p.out}
		cmd.Stderr = cmd.Stdout
		cmd.SysProcAttr = withOwnGroup(cmd.SysProcAttr)
		started := time.Now()
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start %s: %w", p.name, err)
		}
		p.log.Info("started", "process", p.name, "pid", cmd.Process.Pid)
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()

		select {
		case err := <-exited:
			if ctx.Err() != nil {
				return nil
			}
			code := exitCode(err)
			if code == p.refusal && p.refusal != 0 {
				return errRefused{name: p.name, code: code}
			}
			if time.Since(started) > time.Minute {
				backoff = p.minBackoff // it ran for a while: this is a new failure, not the same one
			}
			p.log.Warn("exited; restarting", "process", p.name, "exit", code, "in", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			if backoff *= 2; backoff > p.maxBackoff {
				backoff = p.maxBackoff
			}
		case <-ctx.Done():
			p.stop(cmd, exited)
			return nil
		}
	}
}

// stop asks the process to finish and kills it if it does not.
func (p *proc) stop(cmd *exec.Cmd, exited <-chan error) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(p.grace):
		p.log.Warn("did not stop in time; killing it", "process", p.name, "grace", p.grace.String())
		_ = cmd.Process.Kill()
		<-exited
	}
	p.log.Info("stopped", "process", p.name)
}

// exitCode is the exit code of a finished command, -1 when it did not end by exiting.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// prefixWriter puts a prefix on every line it writes, so that the output of two processes
// can be told apart in one log. Writes of whole lines from different processes do not mix.
type prefixWriter struct {
	mu     sync.Mutex
	prefix string
	w      io.Writer
	rest   []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rest = append(p.rest, b...)
	for {
		i := bytes.IndexByte(p.rest, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(p.prefix), p.rest[:i+1]...)
		if _, err := p.w.Write(line); err != nil {
			return len(b), err
		}
		p.rest = p.rest[i+1:]
	}
	return len(b), nil
}
