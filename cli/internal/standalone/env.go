package standalone

import (
	"net"
	"sort"
	"strings"
)

// Prepared is what Prepare made and the other steps need.
type Prepared struct {
	Layout Layout
	// Listen is the address the control plane listens on (host:port).
	Listen string
	// LocalURL is how the processes on this host reach the control plane.
	LocalURL string
	// PublicURL is how another host does, and what the CLI configuration says.
	PublicURL string
	// AgentListen is where the node on this host serves its local API ("" for its default,
	// 127.0.0.1:9100); a lab node takes a free port, so two labs on one host do not collide.
	AgentListen  string
	AdminKey     string
	NodeToken    string
	AgentToken   string
	NodeID       string
	Fingerprint  string
	CertReplaced bool
	// ControlPlaneAccount is the user the control plane runs as; zero when it runs as the
	// user of this process.
	ControlPlaneAccount account
}

// withEnv returns base with the variables of set (which win) and the defaults of unless (which
// only fill what base does not have). The order of the result is stable.
func withEnv(base []string, set, unless map[string]string) []string {
	have := map[string]int{}
	out := make([]string, 0, len(base)+len(set)+len(unless))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		have[k] = len(out)
		out = append(out, kv)
	}
	put := func(k, v string, overwrite bool) {
		if i, ok := have[k]; ok {
			if overwrite {
				out[i] = k + "=" + v
			}
			return
		}
		have[k] = len(out)
		out = append(out, k+"="+v)
	}
	for _, k := range sortedKeys(set) {
		put(k, set[k], true)
	}
	for _, k := range sortedKeys(unless) {
		put(k, unless[k], false)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ControlPlaneEnv is the environment of the control plane: what this host decides (where it
// listens, its certificate, its keys, the administration key and the node token, the database)
// goes over whatever the environment of asp-server had, except the database, which the operator
// may point at a Postgres of their own. The rest of the settings — the scheduler, the retention —
// come from /etc/asp/server.yaml, which the control plane reads itself.
func ControlPlaneEnv(p Prepared, base []string) []string {
	l := p.Layout
	return withEnv(base, map[string]string{
		"ASP_LISTEN_ADDR":          p.Listen,
		"ASP_TLS_CERT":             l.TLSCert(),
		"ASP_TLS_KEY":              l.TLSKey(),
		"ASP_CLIENT_CA":            l.CACert(),
		"ASP_CA_CERT":              l.CACert(),
		"ASP_CA_KEY":               l.CAKey(),
		"ASP_OIDC_KEY":             l.OIDCKey(),
		"ASP_ATTEST_KEY":           l.AttestKey(),
		"ASP_BOOTSTRAP_API_KEY":    p.AdminKey,
		"ASP_NODE_BOOTSTRAP_TOKEN": p.NodeToken,
		"ASP_AGENT_TOKEN_FILE":     l.AgentToken(),
	}, map[string]string{
		"ASP_DATABASE_URL": "sqlite://" + l.DB(),
	})
}

// Profiles.
const (
	ProfileDefault = "default"
	// ProfileLab runs the node-agent without VMs (--dry-run): the real control plane, the real
	// enrollment and scheduling, and sandboxes that are only records. It is what the smokes and a
	// host without KVM use.
	ProfileLab = "lab"
)

// NodeAgentEnv is the environment of the node on this host. The node-agent reads its settings from
// its flags, its environment and /etc/asp/agent.yaml; these are what asp-server decides, and the
// defaults a node needs when no package left that file.
func NodeAgentEnv(p Prepared, profile string, base []string) []string {
	l := p.Layout
	set := map[string]string{
		"ASP_CONTROL_PLANE_URL":    p.LocalURL,
		"ASP_CONTROL_PLANE_CA":     l.TLSCert(),
		"ASP_NODE_ID":              p.NodeID,
		"ASP_ENROLL":               "1",
		"ASP_NODE_BOOTSTRAP_TOKEN": p.NodeToken,
		"ASP_MTLS":                 "1",
		"ASP_CERT_DIR":             l.NodeCerts(),
		"ASP_DISK_DIR":             l.Disks(),
		"ASP_LOCAL_NET_KEY_DIR":    l.LocalNet(),
		"ASP_AGENT_TOKEN_FILE":     l.AgentToken(),
		"ASP_RECONCILE":            "1",
	}
	if p.AgentListen != "" {
		set["ASP_AGENT_LISTEN"] = p.AgentListen
	}
	unless := map[string]string{}
	if profile == ProfileLab {
		set["ASP_DRY_RUN"] = "1"
		set["ASP_CH_SOCKET_DIR"] = l.Run()
		unless["ASP_RECONCILE_INTERVAL"] = "500ms"
		unless["ASP_HEARTBEAT_INTERVAL"] = "2s"
		unless["ASP_CAPACITY_CPU"] = "4"
		unless["ASP_CAPACITY_MEM_MIB"] = "8192"
		unless["ASP_MAX_SANDBOXES"] = "8"
	} else {
		// What the package puts in /etc/asp/agent.yaml, for a host that has not got it.
		unless["ASP_CH_SOCKET_DIR"] = "/run/asp"
		unless["ASP_TAP_AUTO"] = "1"
		unless["ASP_HOST_VSOCK"] = "1"
		unless["ASP_EGRESS_ENFORCE"] = "1"
		unless["ASP_EGRESS_PROXY_LISTEN"] = ":8888"
		unless["ASP_EGRESS_DNS_SINK"] = ":5353"
	}
	return withEnv(base, set, unless)
}

// hostOf is the host part of host:port ("" for ":8443").
func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}
