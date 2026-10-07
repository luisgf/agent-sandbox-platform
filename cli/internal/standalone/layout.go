// Package standalone is the single-host mode of ASP: one command that makes what a control plane
// and a node need (a TLS certificate, an administration key, a token for the node, a place for
// the state), starts the control plane and a node-agent on this host, keeps them running, and
// leaves the CLI configured to talk to them.
//
// It starts the programs the packages install, as processes: the control plane and the
// node-agent keep their own settings, their own packages and their own tests, and a restart of
// one does not take the other down (the VMs of a node outlive its agent by design).
package standalone

import "path/filepath"

// DefaultDataDir is where a host keeps everything.
const DefaultDataDir = "/var/lib/asp"

// Layout is where a single host keeps its state, all of it under one directory:
//
//	<root>/server/     the control plane: its database, the CA that enrolls nodes, the OIDC and
//	                   attestation keys, the TLS certificate, the administration key, the node token
//	<root>/node-certs  the certificate this host's node enrolled with
//	<root>/disks       the disks of the sandboxes
//	<root>/local-net   the WireGuard keys of local-net sessions
//	<root>/agent.token the secret the control plane sends to the node on this host
type Layout struct{ Root string }

func (l Layout) Server() string          { return filepath.Join(l.Root, "server") }
func (l Layout) File(name string) string { return filepath.Join(l.Server(), name) }
func (l Layout) DB() string              { return l.File("asp.db") }
func (l Layout) TLSCert() string         { return l.File("tls.crt") }
func (l Layout) TLSKey() string          { return l.File("tls.key") }
func (l Layout) CACert() string          { return l.File("ca.crt") }
func (l Layout) CAKey() string           { return l.File("ca.key") }
func (l Layout) OIDCKey() string         { return l.File("oidc-key.pem") }
func (l Layout) AttestKey() string       { return l.File("attest-key.pem") }
func (l Layout) AdminKey() string        { return l.File("admin-key") }
func (l Layout) NodeToken() string       { return l.File("node-token") }
func (l Layout) CLIConfig() string       { return l.File("asp.yaml") }
func (l Layout) AgentToken() string      { return filepath.Join(l.Root, "agent.token") }
func (l Layout) Disks() string           { return filepath.Join(l.Root, "disks") }
func (l Layout) NodeCerts() string       { return filepath.Join(l.Root, "node-certs") }
func (l Layout) LocalNet() string        { return filepath.Join(l.Root, "local-net") }

// Run is a short directory for the sockets of a lab node (unix socket paths are short).
func (l Layout) Run() string { return filepath.Join(l.Root, "run") }
