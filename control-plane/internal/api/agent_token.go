package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// EnvAgentTokenFile names the file with the secret of the node agent's local
// API. The agent creates it (its --agent-token-file, default
// /var/lib/asp/agent.token) and a control plane on the same host reads it and
// sends it as a bearer token with every exec. A control plane on another host
// reaches the agent over mutual TLS and needs none.
const EnvAgentTokenFile = "ASP_AGENT_TOKEN_FILE"

const defaultAgentTokenFile = "/var/lib/asp/agent.token"

// agentTokenSource reads the secret, and reads it again when the file changes,
// so rotating it means replacing the file and restarting the agent.
type agentTokenSource struct {
	mu      sync.Mutex
	path    string // the file value came from
	mod     time.Time
	size    int64
	value   string
	checked time.Time
}

// candidates are the files the secret may be in: the one the operator named,
// or the default and the temporary file a lab agent falls back to when it
// cannot write the default.
func agentTokenCandidates() []string {
	if p := strings.TrimSpace(os.Getenv(EnvAgentTokenFile)); p != "" {
		return []string{p}
	}
	return []string{defaultAgentTokenFile, filepath.Join(os.TempDir(), "asp-agent.token")}
}

// Token returns the secret, or "" when no candidate file is readable.
func (t *agentTokenSource) Token() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.value != "" && now.Sub(t.checked) < time.Second {
		return t.value
	}
	t.checked = now
	for _, p := range agentTokenCandidates() {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if p == t.path && t.value != "" && fi.ModTime().Equal(t.mod) && fi.Size() == t.size {
			return t.value
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			continue
		}
		t.path, t.mod, t.size, t.value = p, fi.ModTime(), fi.Size(), v
		return v
	}
	t.value = ""
	return ""
}

// authorizeAgentRequest adds the agent token to a call to a same-host agent.
// An https:// agent is reached over mutual TLS, which is its credential.
func (s *Server) authorizeAgentRequest(req *http.Request) {
	if req.URL == nil || req.URL.Scheme != "http" {
		return
	}
	if tok := s.agentToken.Token(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}
