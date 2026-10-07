package api

import (
	"fmt"
	"strings"
)

// checkNodeNameFree refuses a node id that is a name of the control plane's own
// TLS certificate (its DNS names, IP addresses and common name, a wildcard name
// covering one label as the certificate does). A node certificate is valid for
// its id, and agents may trust the enrollment CA for the control plane, so a node
// named like the control plane would hold a certificate every agent accepts for
// the control plane's hostname.
func (s *Server) checkNodeNameFree(id string) error {
	for _, name := range s.ReservedNodeNames {
		if certNameCovers(name, id) {
			return fmt.Errorf("node id %q is a name of the control plane's own TLS certificate and cannot be a node", id)
		}
	}
	return nil
}

// certNameCovers reports whether a certificate name (an exact name or a
// single-label wildcard such as *.lab.example.com) is valid for host.
func certNameCovers(name, host string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	host = strings.ToLower(strings.TrimSpace(host))
	if name == "" {
		return false
	}
	if name == host {
		return true
	}
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		label, tail, found := strings.Cut(host, ".")
		return found && label != "" && tail == rest
	}
	return false
}
