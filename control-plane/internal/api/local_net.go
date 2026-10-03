package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/authn/idp"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// HeaderASPCaller marks the caller. Value "guest" cannot set local_net.
const HeaderASPCaller = "X-ASP-Caller"

// HeaderASPGuest is an alias: "1" or "true" means the guest workload.
const HeaderASPGuest = "X-ASP-Guest"

// forbiddenLocalNetKeys are rejected with 400. v1 has no CIDR policy.
var forbiddenLocalNetKeys = []string{
	"local_net_policy",
	"prefixes",
	"cidrs",
	"cidr",
	"ports",
	"routes",
	"exceptions",
	"local_net_allow",
}

func guestCaller(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get(HeaderASPCaller)), "guest") {
		return true
	}
	g := strings.TrimSpace(r.Header.Get(HeaderASPGuest))
	return g == "1" || strings.EqualFold(g, "true")
}

func jsonObjectKeys(body []byte) (map[string]json.RawMessage, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

func forbiddenLocalNetKey(keys map[string]json.RawMessage) string {
	for _, k := range forbiddenLocalNetKeys {
		if _, ok := keys[k]; ok {
			return k
		}
	}
	return ""
}

// allowLocalNetOwner is true for the sandbox owner. A different principal,
// including a tenant admin, cannot take the grant. Lab without an IdP JWT
// is allowed (same as create). Empty owner_sub (legacy rows) is allowed.
func allowLocalNetOwner(p idp.Principal, ok bool, sb store.Sandbox) bool {
	if !ok || strings.TrimSpace(p.Sub) == "" {
		return true
	}
	owner := strings.TrimSpace(sb.OwnerSub)
	if owner == "" {
		return true
	}
	return strings.TrimSpace(p.Sub) == owner
}

type localNetGrantResponse struct {
	Grant     string    `json:"grant"`
	Dial      string    `json:"dial"`
	ExpiresAt time.Time `json:"expires_at"`
	Iface     string    `json:"tunnel_iface"`
	// Transport documents that this slice does not bring the kernel device up.
	Transport string `json:"transport"`
}

type localNetHeartbeatRequest struct {
	Grant           string `json:"grant"`
	ClientPublicKey string `json:"client_public_key"`
}

// IssueLocalNetGrant is POST /v1/sandboxes/{id}/local-net/grant.
// The guest cannot call it. It does not turn the flag on.
func (s *Server) IssueLocalNetGrant(w http.ResponseWriter, r *http.Request) {
	if guestCaller(r) {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, ok := IdPPrincipalFromContext(r.Context())
	if !allowLocalNetOwner(p, ok, sb) {
		writeError(w, http.StatusForbidden, "local_net grant requires owner_sub")
		return
	}
	dial := strings.TrimSpace(os.Getenv("ASP_LOCAL_NET_DIAL"))
	grant, exp, err := s.Store.IssueLocalNetGrant(id, dial, time.Now().UTC(), store.LocalNetGrantTTL)
	if err != nil {
		writeLocalNetErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, localNetGrantResponse{
		Grant:     grant,
		Dial:      dial,
		ExpiresAt: exp,
		Iface:     tunnelIface(id),
		Transport: "wireguard-skeleton",
	})
}

// HeartbeatLocalNet is POST /v1/sandboxes/{id}/local-net/heartbeat.
// A valid grant moves state to up. It does not count as idle activity.
func (s *Server) HeartbeatLocalNet(w http.ResponseWriter, r *http.Request) {
	if guestCaller(r) {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	keys, err := jsonObjectKeys(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if k := forbiddenLocalNetKey(keys); k != "" {
		writeError(w, http.StatusBadRequest, "local_net does not accept "+k+" in v1")
		return
	}
	if _, ok := keys["local_net"]; ok {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	var req localNetHeartbeatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, ok := IdPPrincipalFromContext(r.Context())
	if !allowLocalNetOwner(p, ok, sb) {
		writeError(w, http.StatusForbidden, "local_net heartbeat requires owner_sub")
		return
	}
	out, err := s.Store.HeartbeatLocalNet(id, req.Grant, req.ClientPublicKey, time.Now().UTC())
	if err != nil {
		writeLocalNetErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DetachLocalNet is DELETE /v1/sandboxes/{id}/local-net/attach.
// Egress stays off the public proxy (state withdrawn, flag still true).
func (s *Server) DetachLocalNet(w http.ResponseWriter, r *http.Request) {
	if guestCaller(r) {
		writeError(w, http.StatusForbidden, "guest cannot set local_net")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, ok := IdPPrincipalFromContext(r.Context())
	if !allowLocalNetOwner(p, ok, sb) {
		writeError(w, http.StatusForbidden, "local_net detach requires owner_sub")
		return
	}
	out, err := s.Store.WithdrawLocalNet(id)
	if err != nil {
		writeLocalNetErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeLocalNetErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "sandbox not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func tunnelIface(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 8 {
		id = id[:8]
	}
	if id == "" {
		id = "sandbox"
	}
	return "wg-asp-" + id
}
