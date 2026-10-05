package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/attest"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/fence"
	"github.com/luisgf/agent-sandbox-platform/control-plane/internal/store"
)

type attestRequest struct {
	Statement    attest.BootStatement `json:"statement"`
	Signature    string               `json:"signature"`
	Alg          string               `json:"alg,omitempty"`
	KeyID        string               `json:"key_id,omitempty"`
	PublicKeyPEM string               `json:"public_key_pem,omitempty"`
}

type verifyAttestRequest struct {
	Evidence attest.Evidence `json:"evidence"`
}

// StoreAttestation verifies and persists node-submitted boot evidence.
func (s *Server) StoreAttestation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	if s.Attestor == nil {
		writeError(w, http.StatusServiceUnavailable, "attestation not configured")
		return
	}
	var req attestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Statement.SandboxID) == "" {
		req.Statement.SandboxID = id
	}
	if req.Statement.SandboxID != id {
		writeError(w, http.StatusBadRequest, "sandbox_id mismatch")
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
	if !authorizeSandboxNode(w, r, sb) {
		return
	}
	if certID, ok := NodeIdentityFromContext(r.Context()); ok && req.Statement.NodeID != certID {
		writeError(w, http.StatusForbidden, "attestation statement names node "+req.Statement.NodeID+", not "+certID)
		return
	}
	ev := attest.Evidence{
		Statement:    req.Statement,
		Signature:    req.Signature,
		Alg:          req.Alg,
		KeyID:        req.KeyID,
		PublicKeyPEM: req.PublicKeyPEM,
	}
	if ev.Alg == "" {
		ev.Alg = attest.AlgES256
	}
	if err := s.Attestor.Verify(r.Context(), ev); err != nil {
		writeError(w, http.StatusBadRequest, "attestation verify: "+err.Error())
		return
	}
	ts, _ := time.Parse(time.RFC3339, req.Statement.TS)
	bundle, _ := json.Marshal(ev)
	rec, err := s.Store.PutAttestation(store.PutAttestationInput{
		SandboxID:   id,
		NodeID:      req.Statement.NodeID,
		ImageDigest: req.Statement.ImageDigest,
		VMMProfile:  req.Statement.VMMProfile,
		CID:         req.Statement.CID,
		StatementTS: ts,
		Alg:         ev.Alg,
		KeyID:       ev.KeyID,
		Signature:   ev.Signature,
		Bundle:      bundle,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.EmitEvent(store.EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.attested",
		Actor:     "node-agent",
		Payload:   mustJSONPayload(map[string]any{"node_id": rec.NodeID, "image_digest": rec.ImageDigest, "cid": rec.CID}),
	})
	writeJSON(w, http.StatusOK, rec)
}

// GetAttestation returns the latest stored evidence for a sandbox.
func (s *Server) GetAttestation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	rec, err := s.Store.GetAttestation(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "attestation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	fresh := false
	if s.Attestor != nil {
		ev := attest.Evidence{Statement: attest.BootStatement{
			SandboxID: rec.SandboxID, ImageDigest: rec.ImageDigest, VMMProfile: rec.VMMProfile,
			CID: rec.CID, NodeID: rec.NodeID, TS: rec.StatementTS.UTC().Format(time.RFC3339),
		}}
		fresh = s.Attestor.IsFresh(ev, time.Now().UTC())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"attestation": rec,
		"fresh":       fresh,
	})
}

// VerifyAttestation verifies a bundle without storing it.
func (s *Server) VerifyAttestation(w http.ResponseWriter, r *http.Request) {
	if s.Attestor == nil {
		writeError(w, http.StatusServiceUnavailable, "attestation not configured")
		return
	}
	var req verifyAttestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ev := req.Evidence
	if ev.Alg == "" {
		ev.Alg = attest.AlgES256
	}
	err := s.Attestor.Verify(r.Context(), ev)
	writeJSON(w, http.StatusOK, map[string]any{
		"valid": err == nil,
		"error": errString(err),
		"fresh": err == nil && s.Attestor.IsFresh(ev, time.Now().UTC()),
		"kid":   ev.KeyID,
		"alg":   ev.Alg,
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mustJSONPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// maybeFenceOnReclaim fences the previous owner when reclaiming a running sandbox.
func (s *Server) maybeFenceOnReclaim(ctx context.Context, sb store.Sandbox) {
	if s.Fence == nil || !fence.Enabled() {
		return
	}
	if sb.State != store.SandboxRunning {
		return
	}
	if sb.NodeID == nil || *sb.NodeID == "" {
		return
	}
	now := time.Now().UTC()
	if sb.NodeLeaseUntil != nil && sb.NodeLeaseUntil.After(now) {
		return
	}
	node, err := s.Store.GetNode(*sb.NodeID)
	if err != nil {
		slog.Warn("fence: lookup old node", "node_id", *sb.NodeID, "error", err)
		return
	}
	if strings.TrimSpace(node.FenceEndpoint) == "" {
		slog.Info("fence skipped: no fence_endpoint", "node_id", node.ID, "sandbox_id", sb.ID)
		return
	}
	t := fence.Target{
		NodeID:   node.ID,
		Endpoint: node.FenceEndpoint,
		Token:    node.FenceToken,
	}
	if err := s.Fence.Fence(ctx, t); err != nil {
		if _, ok := err.(fence.SoftFailError); ok {
			slog.Warn("fence soft-fail", "provider", s.Fence.Name(), "node_id", node.ID, "error", err)
			return
		}
		slog.Error("fence failed", "provider", s.Fence.Name(), "node_id", node.ID, "error", err)
		return
	}
	slog.Info("fenced node before reclaim", "provider", s.Fence.Name(), "node_id", node.ID, "sandbox_id", sb.ID)
	_ = s.Store.EmitEvent(store.EmitEventInput{
		SandboxID: sb.ID,
		TenantID:  sb.TenantID,
		EventType: "sandbox.fenced",
		Actor:     "fence",
		Payload:   mustJSONPayload(map[string]any{"old_node_id": node.ID, "provider": s.Fence.Name()}),
	})
}

// attestationClaim builds optional x_asp_attestation for OIDC mint.
func (s *Server) attestationClaim(sandboxID string) map[string]any {
	if s.Attestor == nil {
		return nil
	}
	rec, err := s.Store.GetAttestation(sandboxID)
	if err != nil {
		return nil
	}
	ev := attest.Evidence{Statement: attest.BootStatement{
		SandboxID: rec.SandboxID, ImageDigest: rec.ImageDigest, VMMProfile: rec.VMMProfile,
		CID: rec.CID, NodeID: rec.NodeID, TS: rec.StatementTS.UTC().Format(time.RFC3339),
	}}
	if !s.Attestor.IsFresh(ev, time.Now().UTC()) {
		return nil
	}
	return map[string]any{
		"node_id":      rec.NodeID,
		"image_digest": rec.ImageDigest,
		"vmm_profile":  rec.VMMProfile,
		"cid":          rec.CID,
		"ts":           rec.StatementTS.UTC().Format(time.RFC3339),
		"alg":          rec.Alg,
		"key_id":       rec.KeyID,
	}
}
