package unit

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A service the agent started outlives the agent: it runs in a unit of its own,
// not bound to the agent's. A new agent process takes such a service over
// (Adopted) instead of starting it: it can no longer wait on a systemd-run child,
// so it asks systemd.

// Active reports whether the service name (without the ".service" suffix) is
// running. A service that does not exist is not active.
func (l Launcher) Active(name string) bool {
	return l.command(l.systemctl(), "is-active", "--quiet", name+".service").Run() == nil
}

// Stop stops the service and waits for it. A service that is already gone is not
// an error.
func (l Launcher) Stop(name string) error {
	out, err := l.command(l.systemctl(), "stop", name+".service").CombinedOutput()
	if err != nil && !gone(string(out)) {
		return fmt.Errorf("unit: stop %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// state is what systemd says about a unit.
type state struct {
	Active string // ActiveState: active, activating, deactivating, inactive, failed
	Load   string // LoadState: loaded, not-found…
	Result string // Result: success, signal, exit-code, oom-kill…
	Code   string // ExecMainCode (a number: CLD_EXITED, CLD_KILLED…)
	Status string // ExecMainStatus: the exit status, or the signal
	Pid    int    // MainPID
}

// running says the service still does what it was started for.
func (s state) running() bool {
	switch s.Active {
	case "active", "activating", "reloading", "refreshing", "deactivating":
		return s.Load != "not-found"
	}
	return false
}

func (l Launcher) show(name string) (state, error) {
	out, err := l.command(l.systemctl(), "show",
		"--property=ActiveState,LoadState,Result,ExecMainCode,ExecMainStatus,MainPID", name+".service").Output()
	if err != nil {
		return state{}, err
	}
	var s state
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			s.Active = v
		case "LoadState":
			s.Load = v
		case "Result":
			s.Result = v
		case "ExecMainCode":
			s.Code = v
		case "ExecMainStatus":
			s.Status = v
		case "MainPID":
			fmt.Sscanf(v, "%d", &s.Pid)
		}
	}
	return s, nil
}

// Adopted is a service started by an earlier process, watched by polling systemd.
type Adopted struct {
	l     Launcher
	name  string
	every time.Duration

	mu   sync.Mutex
	pid  int
	done chan struct{}
	kick chan struct{} // a Kill asks Wait to look now
}

// Adopt takes over the service name (without ".service"). Use Active first to
// know it is there: Adopt does not check.
func (l Launcher) Adopt(name string) *Adopted {
	return &Adopted{l: l, name: name, every: time.Second, done: make(chan struct{}), kick: make(chan struct{}, 1)}
}

// Unit is the full name of the service, with its suffix.
func (a *Adopted) Unit() string { return a.name + ".service" }

// SetPollInterval changes how often Wait asks systemd (tests).
func (a *Adopted) SetPollInterval(d time.Duration) { a.every = d }

// Wait blocks until the service is no longer running. A service started with
// --collect is unloaded as soon as it ends, and with it what it ended with, so the
// error says what is known: the result when systemd still has it, else that the
// earlier agent's service ended and nothing more. It returns nil only for a
// service systemd still holds and says succeeded.
func (a *Adopted) Wait() error {
	for {
		s, err := a.l.show(a.name)
		if err == nil {
			a.mu.Lock()
			if s.Pid > 0 {
				a.pid = s.Pid
			}
			a.mu.Unlock()
			if !s.running() {
				return a.ended(s)
			}
		}
		select {
		case <-a.done:
			return errors.New("unit: no longer watched")
		case <-a.kick:
		case <-time.After(a.every):
		}
	}
}

func (a *Adopted) ended(s state) error {
	switch {
	case s.Load == "not-found":
		return errors.New("the service ended and systemd has already forgotten how (it was started by an earlier agent)")
	case s.Result == "success":
		return nil
	}
	return fmt.Errorf("the service ended: result=%s (code %s, status %s)", s.Result, s.Code, s.Status)
}

// Kill sends SIGKILL to every process of the service, like Process.Kill.
func (a *Adopted) Kill() error {
	out, err := a.l.command(a.l.systemctl(), "kill", "--signal=SIGKILL", "--kill-whom=all", a.Unit()).CombinedOutput()
	select {
	case a.kick <- struct{}{}: // do not wait a poll interval to see it end
	default:
	}
	if err != nil && !gone(string(out)) {
		return fmt.Errorf("unit: kill %s: %w: %s", a.Unit(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Pid is the main process of the service, as last seen, or 0.
func (a *Adopted) Pid() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pid > 0 {
		return a.pid
	}
	if s, err := a.l.show(a.name); err == nil && s.Pid > 0 {
		return s.Pid
	}
	return 0
}

// Stop ends the watching without touching the service.
func (a *Adopted) Stop() {
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}
