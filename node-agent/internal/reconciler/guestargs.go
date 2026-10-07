package reconciler

import (
	"fmt"
	"strings"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/tap"
)

// noProxy is what a guest never sends through the egress proxy.
const noProxy = "localhost,127.0.0.1,::1"

// guestNetArgs are the kernel command line arguments that give a guest its network
// identity, appended to the base command line when the sandbox has a TAP:
//
//   - ip=: its address and gateway, its hostname (asp-<shortid>, the name of its TAP)
//     and, when this node's DNS sink answers on the gateway, its resolver. The
//     guest image's cmdline-ip.service applies it (the lab kernel has no IP_PNP),
//     writing /etc/hostname, /etc/hosts and /etc/resolv.conf.
//   - systemd.setenv=: HTTP_PROXY, HTTPS_PROXY and NO_PROXY (both spellings) for
//     everything in the guest, pod-daemon and so the commands it runs, when the
//     node has an egress proxy. With the nft redirect on the proxy is not optional;
//     this is what makes the tools that honour the variable use it on their own.
//
// A guest without a resolver gets none, rather than one that swallows queries: a
// lookup then fails at once instead of timing out.
func guestNetArgs(sandboxID string, g tap.GuestNet, dns bool, proxyPort int) string {
	resolver := ""
	if dns {
		resolver = g.Host.String()
	}
	args := []string{g.KernelIPArg(tap.DeviceName(sandboxID), resolver)}
	if proxyPort > 0 {
		proxy := fmt.Sprintf("http://%s:%d", g.Host, proxyPort)
		for _, kv := range [][2]string{
			{"HTTP_PROXY", proxy}, {"HTTPS_PROXY", proxy}, {"NO_PROXY", noProxy},
			{"http_proxy", proxy}, {"https_proxy", proxy}, {"no_proxy", noProxy},
		} {
			args = append(args, "systemd.setenv="+kv[0]+"="+kv[1])
		}
	}
	return strings.Join(args, " ")
}
