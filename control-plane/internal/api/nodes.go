package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// nodeView is a node as operators see it: what is placed on it, what it offers
// and whether new sandboxes can go there. Fence credentials are never included.
type nodeView struct {
	store.Node
	// Allocated memory includes the per-VM overhead (VMOverheadMiB per sandbox).
	Allocated store.NodeUsage `json:"allocated"`
	// Allocatable is what the node offers after CPU overcommit; 0 = not enforced.
	Allocatable   store.NodeUsage `json:"allocatable"`
	VMOverheadMiB int64           `json:"vm_overhead_mib"`
	// StoppedSandboxes counts the stopped sandboxes on the node: each keeps a
	// disk there and holds no CPU or memory (ADR-0012).
	StoppedSandboxes int64 `json:"stopped_sandboxes"`
	// FenceConfigured says the control plane can power the node off when it
	// declares it lost. The target itself is never serialized.
	FenceConfigured     bool   `json:"fence_configured"`
	Schedulable         bool   `json:"schedulable"`
	UnschedulableReason string `json:"unschedulable_reason,omitempty"`
}

type listNodesResponse struct {
	Nodes []nodeView `json:"nodes"`
}

func (s *Server) nodeView(n store.Node, u store.NodeUsage, stopped int64, now time.Time) nodeView {
	c := store.Candidate(n, u)
	cpu, mem, slots := sched.Allocatable(s.Sched, c)
	reason := sched.Unschedulable(s.Sched, c, now)
	allocated := u
	allocated.MemoryMiB = sched.MemoryUsed(s.Sched, c)
	return nodeView{
		Node:                n,
		Allocated:           allocated,
		Allocatable:         store.NodeUsage{CPUMillis: cpu, MemoryMiB: mem, Sandboxes: slots},
		VMOverheadMiB:       s.Sched.VMOverheadMiB,
		StoppedSandboxes:    stopped,
		FenceConfigured:     strings.TrimSpace(n.FenceEndpoint) != "",
		Schedulable:         reason == "",
		UnschedulableReason: string(reason),
	}
}

// ListNodes returns registered nodes with their allocation, sorted by id. It
// needs an IdP admin or operator, or a platform-scoped API key.
func (s *Server) ListNodes(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeView(w, r) {
		return
	}
	list, err := s.Store.ListNodes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, err := s.Store.ListNodeUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stopped, err := s.Store.CountStoppedByNode(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	now := time.Now().UTC()
	out := make([]nodeView, 0, len(list))
	for _, n := range list {
		out = append(out, s.nodeView(n, usage[n.ID], stopped[n.ID], now))
	}
	writeJSON(w, http.StatusOK, listNodesResponse{Nodes: out})
}

// CordonNode stops new placements on a node; running sandboxes stay.
func (s *Server) CordonNode(w http.ResponseWriter, r *http.Request) {
	s.setNodeCordoned(w, r, true)
}

// UncordonNode lets the scheduler place sandboxes on the node again.
func (s *Server) UncordonNode(w http.ResponseWriter, r *http.Request) {
	s.setNodeCordoned(w, r, false)
}

// setFenceRequest is the body of PUT /v1/nodes/{id}/fence.
type setFenceRequest struct {
	// Endpoint is where the control plane asks for the power-off: a webhook URL,
	// a Redfish base URL or an IPMI host, according to ASP_FENCE_PROVIDER.
	Endpoint string `json:"endpoint"`
	// Token is the credential for it: the secret, or a reference the control
	// plane resolves when it fences ("env:NAME", "file:/abs/path").
	Token string `json:"token,omitempty"`
}

// SetNodeFence says how the control plane powers a node off when it declares it
// lost. Only an admin may: a node that chose its own target could have the
// control plane power off another host.
func (s *Server) SetNodeFence(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeAdmin(w, r, "configure node fencing", "an idp admin token or a platform api key is required to configure node fencing") {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	var req setFenceRequest
	if !decodeJSON(w, r, maxSmallBodyBytes, &req) {
		return
	}
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.Token = strings.TrimSpace(req.Token)
	if req.Endpoint == "" || len(req.Endpoint) > 2048 || strings.ContainsAny(req.Endpoint, "\r\n\x00") {
		writeError(w, http.StatusBadRequest, "endpoint required (at most 2048 characters, no control characters)")
		return
	}
	if err := fence.ValidateSecretRef(req.Token); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.storeNodeFence(w, r, id, req.Endpoint, req.Token)
}

// ClearNodeFence removes a node's fence target.
func (s *Server) ClearNodeFence(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeAdmin(w, r, "configure node fencing", "an idp admin token or a platform api key is required to configure node fencing") {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	s.storeNodeFence(w, r, id, "", "")
}

func (s *Server) storeNodeFence(w http.ResponseWriter, r *http.Request, id, endpoint, token string) {
	if _, err := s.Store.SetNodeFence(r.Context(), id, endpoint, token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The log says that a target changed and who changed it, never what it is.
	slog.Info("node fence target changed", "node_id", id, "configured", endpoint != "", "actor_sub", resolveActorSub(r, "", ""))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setNodeCordoned(w http.ResponseWriter, r *http.Request, cordoned bool) {
	if !authorizeNodeAdmin(w, r, "cordon or uncordon nodes", "") {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	n, err := s.Store.SetNodeCordoned(r.Context(), id, cordoned)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, err := s.Store.ListNodeUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stopped, err := s.Store.CountStoppedByNode(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("node cordon changed", "node_id", id, "cordoned", cordoned, "actor_sub", resolveActorSub(r, "", ""))
	writeJSON(w, http.StatusOK, s.nodeView(n, usage[id], stopped[id], time.Now().UTC()))
}

// doctorTimeout bounds a node's self-checks: a dozen probes of a few seconds each.
const doctorTimeout = 90 * time.Second

// NodeDoctor asks the node-agent of a node to run its self-checks (KVM, hypervisor,
// guest images, disk, nftables, clock, this control plane) and returns its report as
// the agent made it. It reaches the node the way an exec does, over mTLS or the
// loopback agent token, so what it finds is what the node would tell an operator at
// its console. For the callers that may list nodes.
func (s *Server) NodeDoctor(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeView(w, r) {
		return
	}
	id := r.PathValue("id")
	node, err := s.Store.GetNode(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if node.RevokedAt != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("node %s is revoked", node.ID))
		return
	}
	baseURL, client, status, msg := s.nodeAgentTarget(node)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), doctorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/internal/doctor", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.authorizeAgentRequest(req)
	// The checks take longer than the wait for response headers that bounds the
	// calls to an agent.
	resp, err := s.bufferedTwin(client).Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "node-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read node-agent doctor response: "+err.Error())
		return
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
		// An agent from before the doctor has no such route.
		writeError(w, http.StatusNotImplemented, "this node-agent has no doctor: upgrade it")
		return
	}
	if resp.StatusCode >= 300 {
		writeAgentError(w, resp.StatusCode, body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
