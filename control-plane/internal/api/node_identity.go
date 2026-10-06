package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

// Node-agent routes act for one node. With mTLS on, the node is the one named by the
// client certificate (NodeIdentityFromContext), so an enrolled node cannot claim,
// report or mint tokens for another node's sandboxes. Without a verified node
// certificate (open lab, API key) these helpers allow the request, as before.

// actingNodeID returns the node id a node-agent request acts for: the body value, or
// the certificate's node when the body leaves it empty.
func actingNodeID(r *http.Request, fromBody string) string {
	if id := strings.TrimSpace(fromBody); id != "" {
		return id
	}
	certID, _ := NodeIdentityFromContext(r.Context())
	return certID
}

// authorizeNodeID writes 403 and returns false when the certificate names another node.
func authorizeNodeID(w http.ResponseWriter, r *http.Request, nodeID string) bool {
	certID, ok := NodeIdentityFromContext(r.Context())
	if !ok {
		return true
	}
	if strings.TrimSpace(nodeID) != certID {
		writeError(w, http.StatusForbidden, fmt.Sprintf("client certificate is for node %q, not %q", certID, nodeID))
		return false
	}
	return true
}

// authorizeSandboxNode writes 403 and returns false when the sandbox is not assigned
// to the node named by the certificate.
func authorizeSandboxNode(w http.ResponseWriter, r *http.Request, sb store.Sandbox) bool {
	certID, ok := NodeIdentityFromContext(r.Context())
	if !ok {
		return true
	}
	if sb.NodeID == nil || *sb.NodeID != certID {
		writeError(w, http.StatusForbidden, fmt.Sprintf("sandbox %s is not assigned to node %q", sb.ID, certID))
		return false
	}
	return true
}

// authorizeSandboxNodeByID loads the sandbox only when a node identity is present.
func (s *Server) authorizeSandboxNodeByID(w http.ResponseWriter, r *http.Request, id string) bool {
	if _, ok := NodeIdentityFromContext(r.Context()); !ok {
		return true
	}
	sb, err := s.Store.GetSandbox(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return false
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	return authorizeSandboxNode(w, r, sb)
}
