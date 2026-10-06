// Package certrenew renews the node certificate before it expires: the node
// asks the control plane for a new one, authenticated by the current one over
// mTLS, writes it to cert-dir and starts presenting it without a restart.
package certrenew

import (
	"context"
	"crypto/x509"
	"log/slog"
	"time"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/cpclient"
)

const (
	// DefaultEvery is how often the certificate is checked.
	DefaultEvery = 24 * time.Hour
	// warnWithin: a certificate this close to expiry that fails to renew is
	// logged as a warning on every check.
	warnWithin = 30 * 24 * time.Hour
)

// Renewer renews the node certificate when less than a third of its lifetime
// is left.
type Renewer struct {
	CP     *cpclient.Client // the node's mTLS client
	Cert   *cpclient.NodeCert
	NodeID string
	Logger *slog.Logger
	Every  time.Duration
	// OnRotate runs after the new certificate is loaded, e.g. to close idle
	// connections so that new ones present it.
	OnRotate func()
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
}

// Due reports whether leaf has less than a third of its lifetime left at now.
func Due(leaf *x509.Certificate, now time.Time) bool {
	life := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Sub(now) < life/3
}

// Check renews the certificate when it is due and reports whether it did.
func (r *Renewer) Check(ctx context.Context) (bool, error) {
	leaf := r.Cert.Leaf()
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if !Due(leaf, now) {
		return false, nil
	}
	resp, err := r.CP.RotateCert(ctx, r.NodeID)
	if err == nil {
		err = cpclient.WriteCerts(r.Cert.Dir(), resp.ClientCertPEM, resp.ClientKeyPEM, resp.CACertPEM)
	}
	if err == nil {
		err = r.Cert.Reload()
	}
	if err != nil {
		left := leaf.NotAfter.Sub(now)
		switch {
		case left <= 0:
			r.log().Error("node certificate expired and could not be renewed: the control plane refuses this node until it re-enrolls",
				"not_after", leaf.NotAfter, "error", err)
		case left < warnWithin:
			r.log().Warn("node certificate expires soon and could not be renewed",
				"not_after", leaf.NotAfter, "days_left", int(left.Hours()/24), "error", err)
		}
		return false, err
	}
	if r.OnRotate != nil {
		r.OnRotate()
	}
	r.log().Info("node certificate renewed", "not_after", r.Cert.Leaf().NotAfter, "fingerprint", resp.CertFingerprint)
	return true, nil
}

// Run checks at once and then every Every until ctx ends.
func (r *Renewer) Run(ctx context.Context) {
	every := r.Every
	if every <= 0 {
		every = DefaultEvery
	}
	check := func() {
		if _, err := r.Check(ctx); err != nil && ctx.Err() == nil {
			r.log().Warn("node certificate renewal failed; retrying later", "every", every, "error", err)
		}
	}
	check()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

func (r *Renewer) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
