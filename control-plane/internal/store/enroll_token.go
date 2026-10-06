package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnrollToken is a single-use node enrollment token issued by an admin
// (POST /v1/nodes/enroll-tokens). Only the SHA-256 of the token is stored.
type EnrollToken struct {
	Hash string `json:"-"`
	// NodeID pins the token: only this node id may enroll with it, and only a
	// pinned token may re-enroll a node that holds a certificate.
	NodeID    string     `json:"node_id,omitempty"`
	ExpiresAt time.Time  `json:"expires_at"`
	UsedAt    *time.Time `json:"used_at,omitempty"`
	UsedBy    string     `json:"used_by,omitempty"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// EnrollAuth is what authorized an enrollment.
type EnrollAuth struct {
	// TokenHash is the SHA-256 of a single-use enroll token; EnrollNode marks
	// it used in the same transaction. Empty means the shared bootstrap token
	// (ASP_NODE_BOOTSTRAP_TOKEN).
	TokenHash string
}

var (
	// ErrEnrollTokenInvalid: the enroll token is unknown, used or expired.
	ErrEnrollTokenInvalid = errors.New("enroll token unknown, used or expired")
	// ErrEnrollTokenPinned: the enroll token is pinned to another node id.
	ErrEnrollTokenPinned = errors.New("enroll token is pinned to another node")
	// ErrNodeEnrolled: the node id holds a certificate and is not revoked, and
	// the enrollment was not authorized by a token pinned to it.
	ErrNodeEnrolled = errors.New("node is enrolled and not revoked")
)

// enrollTokenRetention is how long expired tokens stay for the audit trail.
const enrollTokenRetention = 7 * 24 * time.Hour

// HashEnrollToken returns the stored form of a raw enroll token.
func HashEnrollToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

func validateEnrollToken(tok EnrollToken) error {
	if strings.TrimSpace(tok.Hash) == "" {
		return fmt.Errorf("%w: enroll token hash required", ErrInvalidInput)
	}
	if tok.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: enroll token expiry required", ErrInvalidInput)
	}
	return nil
}

// enrolledLive reports whether a node holds a certificate and is not revoked:
// enrolling it again would revoke the certificate it uses.
func enrolledLive(n Node) bool {
	return n.RevokedAt == nil && strings.TrimSpace(n.CertFingerprint) != ""
}

// enrollAllowed decides an enrollment of node id. tok is the enroll token
// (nil for the bootstrap token); live is enrolledLive of the existing node.
//
//   - A token must be unused and unexpired, and if pinned, pinned to id.
//   - A node id that holds a live certificate is only re-enrolled with a
//     token pinned to it: the bootstrap token every node holds, or an
//     unpinned token, must not take over a node and revoke its certificate.
func enrollAllowed(id string, tok *EnrollToken, live bool, now time.Time) error {
	if tok != nil {
		if tok.UsedAt != nil || !tok.ExpiresAt.After(now) {
			return ErrEnrollTokenInvalid
		}
		if tok.NodeID != "" && tok.NodeID != id {
			return fmt.Errorf("%w (%s)", ErrEnrollTokenPinned, tok.NodeID)
		}
	}
	if live && (tok == nil || tok.NodeID != id) {
		return fmt.Errorf("%w: %s", ErrNodeEnrolled, id)
	}
	return nil
}

func enrollAuthName(auth EnrollAuth) string {
	if auth.TokenHash != "" {
		return "enroll_token"
	}
	return "bootstrap_token"
}
