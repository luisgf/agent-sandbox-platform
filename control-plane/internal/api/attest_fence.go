package api

import (
	"context"
	"crypto/ecdsa"
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
	if !decodeJSON(w, r, maxSmallBodyBytes, &req) {
		return
	}
	if strings.TrimSpace(req.Statement.SandboxID) == "" {
		req.Statement.SandboxID = id
	}
	if req.Statement.SandboxID != id {
		writeError(w, http.StatusBadRequest, "sandbox_id mismatch")
		return
	}
	sb, err := s.Store.GetSandbox(r.Context(), id)
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
	if err := s.Attestor.VerifyWithNodeKey(r.Context(), ev, peerNodeKey(r)); err != nil {
		writeError(w, http.StatusBadRequest, "attestation verify: "+err.Error())
		return
	}
	var image attest.AllowedImage
	if s.AttestAllow != nil {
		var err error
		if image, err = s.AttestAllow.Check(req.Statement); err != nil {
			_ = s.Store.EmitEvent(r.Context(), store.EmitEventInput{
				SandboxID: id, TenantID: sb.TenantID, EventType: "sandbox.attestation_refused", Actor: "node-agent",
				Payload: mustJSONPayload(map[string]any{"node_id": req.Statement.NodeID, "reason": err.Error()}),
			})
			writeError(w, http.StatusBadRequest, "attestation refused: "+err.Error())
			return
		}
	}
	ts, _ := time.Parse(time.RFC3339, req.Statement.TS)
	bundle, _ := json.Marshal(ev)
	rec, err := s.Store.PutAttestation(r.Context(), store.PutAttestationInput{
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
	_ = s.Store.EmitEvent(r.Context(), store.EmitEventInput{
		SandboxID: id,
		TenantID:  sb.TenantID,
		EventType: "sandbox.attested",
		Actor:     "node-agent",
		Payload:   mustJSONPayload(attestedPayload(rec, req.Statement, image)),
	})
	writeJSON(w, http.StatusOK, rec)
}

// peerNodeKey returns the ECDSA key of the node certificate a request was
// authenticated with (mTLS, ASP_CLIENT_CA), or nil. StoreAttestation already
// checked that the certificate names the statement's node, so a signature by
// this key binds the evidence to that node's enrolled identity.
func peerNodeKey(r *http.Request) *ecdsa.PublicKey {
	if _, ok := NodeIdentityFromContext(r.Context()); !ok || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	pub, _ := r.TLS.PeerCertificates[0].PublicKey.(*ecdsa.PublicKey)
	return pub
}

// GetAttestation returns the latest stored evidence for a sandbox.
func (s *Server) GetAttestation(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "sandbox id required")
		return
	}
	sb, err := s.Store.GetSandbox(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !sandboxVisible(w, r, sb) {
		return
	}
	rec, err := s.Store.GetAttestation(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "attestation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stmt := statementOf(rec)
	fresh := false
	if s.Attestor != nil {
		fresh = s.Attestor.IsFresh(attest.Evidence{Statement: stmt}, time.Now().UTC())
	}
	resp := map[string]any{
		"attestation": rec,
		"fresh":       fresh,
		"measured":    stmt.Measured(),
	}
	if s.AttestAllow != nil {
		img, err := s.AttestAllow.Check(stmt)
		resp["allowlisted"] = err == nil
		if err == nil {
			resp["image_name"] = img.Name
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// statementOf is the boot statement behind a stored record: the signed
// statement kept in its bundle, so the fields the record has no column for
// (kernel digest, hypervisor version, boot kind) are there. Without a readable
// bundle it is rebuilt from the record's columns.
func statementOf(rec store.AttestationRecord) attest.BootStatement {
	var ev attest.Evidence
	if err := json.Unmarshal(rec.Bundle, &ev); err == nil && ev.Statement.SandboxID == rec.SandboxID {
		return ev.Statement
	}
	return attest.BootStatement{
		SandboxID: rec.SandboxID, ImageDigest: rec.ImageDigest, VMMProfile: rec.VMMProfile,
		CID: rec.CID, NodeID: rec.NodeID, TS: rec.StatementTS.UTC().Format(time.RFC3339),
	}
}

// attestedPayload is the journal entry for an accepted statement.
func attestedPayload(rec store.AttestationRecord, st attest.BootStatement, image attest.AllowedImage) map[string]any {
	p := map[string]any{"node_id": rec.NodeID, "image_digest": rec.ImageDigest, "cid": rec.CID, "measured": st.Measured()}
	if st.KernelDigest != "" {
		p["kernel_digest"] = st.KernelDigest
	}
	if st.VMMVersion != "" {
		p["vmm_version"] = st.VMMVersion
	}
	if st.Boot != "" {
		p["boot"] = st.Boot
	}
	if image.Name != "" {
		p["image_name"] = image.Name
	}
	return p
}

// VerifyAttestation verifies a bundle without storing it.
func (s *Server) VerifyAttestation(w http.ResponseWriter, r *http.Request) {
	if s.Attestor == nil {
		writeError(w, http.StatusServiceUnavailable, "attestation not configured")
		return
	}
	var req verifyAttestRequest
	if !decodeJSON(w, r, maxSmallBodyBytes, &req) {
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

// attestationClaim builds optional x_asp_attestation for OIDC mint.
//
// The claim says what the node measured, not that the control plane checked
// it: "measured" is whether the statement carries real digests of the kernel
// and the base image, and "allowlisted" is whether the control plane has an
// image allowlist and the digests are on it. The digests are the node's own
// hashing, signed by the node: a node the control plane trusts, not a hardware
// root of trust. A statement from a node that does not measure carries no
// digests here, only who signed it and when.
//
// With an allowlist, evidence for an image that is no longer on it yields no
// claim at all.
func (s *Server) attestationClaim(ctx context.Context, sandboxID string) map[string]any {
	if s.Attestor == nil {
		return nil
	}
	rec, err := s.Store.GetAttestation(ctx, sandboxID)
	if err != nil {
		return nil
	}
	stmt := statementOf(rec)
	if !s.Attestor.IsFresh(attest.Evidence{Statement: stmt}, time.Now().UTC()) {
		return nil
	}
	claim := map[string]any{
		"node_id":     rec.NodeID,
		"vmm_profile": rec.VMMProfile,
		"cid":         rec.CID,
		"ts":          rec.StatementTS.UTC().Format(time.RFC3339),
		"alg":         rec.Alg,
		"key_id":      rec.KeyID,
		"measured":    stmt.Measured(),
		"allowlisted": false,
	}
	if s.AttestAllow != nil {
		img, err := s.AttestAllow.Check(stmt)
		if err != nil {
			return nil
		}
		claim["allowlisted"] = true
		claim["image_name"] = img.Name
	}
	if stmt.Measured() {
		claim["image_digest"] = stmt.ImageDigest
		claim["kernel_digest"] = stmt.KernelDigest
		if stmt.VMMVersion != "" {
			claim["vmm_version"] = stmt.VMMVersion
		}
		if stmt.Boot != "" {
			claim["boot"] = stmt.Boot
		}
	}
	return claim
}

// fenceNode powers off a lost node through the configured FenceProvider before
// its sandboxes are failed, so a partitioned node cannot keep running them. It
// reports false when fencing is off or the node has no fence endpoint.
func (s *Server) fenceNode(ctx context.Context, n store.Node) (bool, error) {
	if s.Fence == nil || !fence.Enabled() {
		return false, nil
	}
	if strings.TrimSpace(n.FenceEndpoint) == "" {
		slog.Info("fence skipped: no fence_endpoint", "node_id", n.ID)
		return false, nil
	}
	token, err := fence.ResolveSecret(n.FenceToken)
	if err != nil {
		return false, err
	}
	if err := s.Fence.Fence(ctx, fence.Target{NodeID: n.ID, Endpoint: n.FenceEndpoint, Token: token}); err != nil {
		return false, err
	}
	return true, nil
}
