package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/pki"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

const (
	defaultEnrollTokenTTL = time.Hour
	maxEnrollTokenTTL     = 7 * 24 * time.Hour
	// enrollTokenPrefix marks enroll tokens in logs and secret scanners.
	enrollTokenPrefix = "asp_enroll_"
)

type enrollTokenRequest struct {
	NodeID     string `json:"node_id"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type enrollTokenResponse struct {
	Token     string    `json:"token"`
	NodeID    string    `json:"node_id,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note"`
}

// CreateEnrollToken issues a single-use node enrollment token (IdP admin or
// platform-scoped API key). With node_id the token is pinned: only that node
// may enroll with it, and it may re-enroll a node that holds a certificate.
func (s *Server) CreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	if !authorizeNodeAdmin(w, r, "issue enroll tokens", "an idp admin token or a platform api key is required to issue enroll tokens") {
		return
	}
	var body enrollTokenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	nodeID := strings.TrimSpace(body.NodeID)
	if nodeID != "" {
		if err := pki.ValidNodeID(nodeID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	ttl := defaultEnrollTokenTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if ttl > maxEnrollTokenTTL {
		writeError(w, http.StatusBadRequest, "ttl_seconds too long: at most 7 days")
		return
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "token: "+err.Error())
		return
	}
	raw := enrollTokenPrefix + hex.EncodeToString(b[:])
	now := time.Now().UTC()
	actor := adminActor(r)
	tok := store.EnrollToken{
		Hash:      store.HashEnrollToken(raw),
		NodeID:    nodeID,
		ExpiresAt: now.Add(ttl),
		CreatedBy: actor,
		CreatedAt: now,
	}
	if err := s.Store.CreateEnrollToken(tok); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slog.Info("node enroll token issued", "node_id", nodeID, "expires_at", tok.ExpiresAt.Format(time.RFC3339), "created_by", actor)
	note := "single use; enroll with: node-agent --enroll --enroll-token=<token> --node-id=<id>"
	if nodeID != "" {
		note = "single use, only for node " + nodeID + " (it may re-enroll a node that is enrolled); enroll with: node-agent --enroll --enroll-token=<token> --node-id=" + nodeID
	}
	writeJSON(w, http.StatusCreated, enrollTokenResponse{Token: raw, NodeID: nodeID, ExpiresAt: tok.ExpiresAt, Note: note})
}

// adminActor names who called a node admin route, for logs and audit rows.
func adminActor(r *http.Request) string {
	if p, ok := IdPPrincipalFromContext(r.Context()); ok && strings.TrimSpace(p.Sub) != "" {
		return strings.TrimSpace(p.Sub)
	}
	if k, ok := APIKeyFromContext(r.Context()); ok {
		return "api-key:" + k.Name
	}
	return "open-lab"
}

// enrollAuth reads the enroll credential: the shared bootstrap token, or any
// other bearer as a single-use enroll token (checked by the store).
func enrollAuth(w http.ResponseWriter, r *http.Request) (store.EnrollAuth, bool) {
	if checkBootstrapToken(r) {
		return store.EnrollAuth{}, true
	}
	raw := bearerToken(r.Header.Get("Authorization"))
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("X-ASP-Bootstrap-Token"))
	}
	if raw == "" {
		writeError(w, http.StatusUnauthorized, "bootstrap token or enroll token required")
		return store.EnrollAuth{}, false
	}
	return store.EnrollAuth{TokenHash: store.HashEnrollToken(raw)}, true
}

func enrollAuthLabel(auth store.EnrollAuth) string {
	if auth.TokenHash != "" {
		return "enroll_token"
	}
	return "bootstrap_token"
}

func writeEnrollError(w http.ResponseWriter, nodeID string, err error) {
	switch {
	case errors.Is(err, store.ErrEnrollTokenInvalid):
		// A wrong bootstrap token lands here too: say so without telling which.
		writeError(w, http.StatusUnauthorized, "invalid bootstrap token, or enroll token unknown, used or expired")
	case errors.Is(err, store.ErrEnrollTokenPinned):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrNodeEnrolled):
		writeError(w, http.StatusConflict, "node "+nodeID+" is enrolled and not revoked: re-enrolling it would revoke the certificate it uses. "+
			"Re-key it with an enroll token pinned to it (asp node enroll-token --node-id "+nodeID+"), or revoke it first")
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
