package sshagent

import (
	"strings"
	"sync"
)

// Registry maps sandboxID → host SSH agent unix path for per-sandbox upstream
// selection (ADR-0007 phase 4). When a template is configured, Bind expands it
// with owner_sub / sandbox_id; missing sockets → FakeAgent at dial time.
//
// Honest limit: this is path injection / ops wiring, not a per-user ssh-agent
// daemon manager. Keys must be provisioned outside ASP.
type Registry struct {
	mu sync.RWMutex
	// bySandbox stores explicit bindings. Presence of a key (even with empty
	// path) means "scoped": ServeConn must not fall back to process SSH_AUTH_SOCK.
	bySandbox map[string]string

	// Template is ASP_SSH_AGENT_SOCK_TEMPLATE, e.g.
	//   /run/asp/ssh-agents/{owner_sub}.sock
	//   /run/asp/ssh-agents/{owner_sub}/{sandbox_id}.sock
	// Placeholders: {owner_sub}, {sandbox_id}, {id} (alias of sandbox_id).
	Template string

	// Fallback is the legacy node-wide sock (SSH_AUTH_SOCK / bridge upstream)
	// used when Template is empty. Empty Fallback + no template → FakeAgent.
	Fallback string
}

// NewRegistry returns an empty registry.
func NewRegistry(template, fallback string) *Registry {
	return &Registry{
		bySandbox: make(map[string]string),
		Template:  strings.TrimSpace(template),
		Fallback:  strings.TrimSpace(fallback),
	}
}

// ExpandSockTemplate replaces placeholders. Empty owner_sub leaves an empty
// path segment (ops should provision or accept FakeAgent).
func ExpandSockTemplate(tmpl, ownerSub, sandboxID string) string {
	out := tmpl
	out = strings.ReplaceAll(out, "{owner_sub}", ownerSub)
	out = strings.ReplaceAll(out, "{sandbox_id}", sandboxID)
	out = strings.ReplaceAll(out, "{id}", sandboxID)
	return out
}

// Bind records the upstream path for a sandbox and returns it.
// Template set → expand (no silent fall-back to global Fallback — that would
// reintroduce cross-user key sharing). Template empty → Fallback (lab/legacy).
func (r *Registry) Bind(sandboxID, ownerSub string) string {
	if r == nil || sandboxID == "" {
		return ""
	}
	path := ""
	if r.Template != "" {
		path = ExpandSockTemplate(r.Template, strings.TrimSpace(ownerSub), sandboxID)
	} else {
		path = r.Fallback
	}
	r.Set(sandboxID, path)
	return path
}

// Set stores an explicit path (empty = intentional FakeAgent, scoped).
func (r *Registry) Set(sandboxID, path string) {
	if r == nil || sandboxID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bySandbox == nil {
		r.bySandbox = make(map[string]string)
	}
	r.bySandbox[sandboxID] = path
}

// Unset removes a sandbox binding (call on Detach/Stop).
func (r *Registry) Unset(sandboxID string) {
	if r == nil || sandboxID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bySandbox, sandboxID)
}

// Get returns (path, true) when the sandbox is bound (path may be empty).
func (r *Registry) Get(sandboxID string) (string, bool) {
	if r == nil || sandboxID == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.bySandbox[sandboxID]
	return p, ok
}

// Lookup returns the bound path, or Fallback if unbound and no template mode,
// or "" (FakeAgent) when template mode and unbound / empty bind.
func (r *Registry) Lookup(sandboxID string) string {
	if r == nil {
		return ""
	}
	if p, ok := r.Get(sandboxID); ok {
		return p
	}
	if r.Template != "" {
		return ""
	}
	return r.Fallback
}

// Scoped reports whether ServeConn must avoid process-env SSH_AUTH_SOCK fallback
// for this sandbox (bound entry or template mode active).
func (r *Registry) Scoped(sandboxID string) bool {
	if r == nil {
		return false
	}
	if _, ok := r.Get(sandboxID); ok {
		return true
	}
	return r.Template != ""
}

// Len returns bound entry count (tests).
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.bySandbox)
}
