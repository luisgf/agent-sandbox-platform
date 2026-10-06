package api

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/sched"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// nodeView is a node as operators see it: what is placed on it, what it offers
// and whether new sandboxes can go there. Fence credentials are never included.
type nodeView struct {
	store.Node
	Allocated store.NodeUsage `json:"allocated"`
	// Allocatable is what the node offers after CPU overcommit; 0 = not enforced.
	Allocatable         store.NodeUsage `json:"allocatable"`
	Schedulable         bool            `json:"schedulable"`
	UnschedulableReason string          `json:"unschedulable_reason,omitempty"`
}

type listNodesResponse struct {
	Nodes []nodeView `json:"nodes"`
}

func (s *Server) nodeView(n store.Node, u store.NodeUsage, now time.Time) nodeView {
	c := store.Candidate(n, u)
	cpu, mem, slots := sched.Allocatable(s.Sched, c)
	reason := sched.Unschedulable(s.Sched, c, now)
	return nodeView{
		Node:                n,
		Allocated:           u,
		Allocatable:         store.NodeUsage{CPUMillis: cpu, MemoryMiB: mem, Sandboxes: slots},
		Schedulable:         reason == "",
		UnschedulableReason: string(reason),
	}
}

// ListNodes returns registered nodes with their allocation, sorted by id. With an
// IdP principal it needs admin or operator.
func (s *Server) ListNodes(w http.ResponseWriter, r *http.Request) {
	if p, ok := IdPPrincipalFromContext(r.Context()); ok && !canViewNodes(p) {
		forbid(w, "admin or operator role required to list nodes")
		return
	}
	list, err := s.Store.ListNodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, err := s.Store.ListNodeUsage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	now := time.Now().UTC()
	out := make([]nodeView, 0, len(list))
	for _, n := range list {
		out = append(out, s.nodeView(n, usage[n.ID], now))
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

func (s *Server) setNodeCordoned(w http.ResponseWriter, r *http.Request, cordoned bool) {
	if p, ok := IdPPrincipalFromContext(r.Context()); ok && !canManageNodes(p) {
		forbid(w, "admin role required to cordon or uncordon nodes")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "node id required")
		return
	}
	n, err := s.Store.SetNodeCordoned(id, cordoned)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	usage, err := s.Store.ListNodeUsage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("node cordon changed", "node_id", id, "cordoned", cordoned, "actor_sub", resolveActorSub(r, "", ""))
	writeJSON(w, http.StatusOK, s.nodeView(n, usage[id], time.Now().UTC()))
}
