// Optional lab binary: HTTP forward proxy allow/deny smoke (deny-by-default).
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8888", "proxy listen address")
	allow := flag.String("allow", "127.0.0.1", "comma-separated allowlist entries, host or host:port (a host alone is for ports 80 and 443)")
	allowLoopback := flag.Bool("allow-loopback", false, "let the proxy connect to loopback upstreams (this smoke only: the node-agent never does)")
	flag.Parse()
	al := egress.NewAllowlist()
	for _, h := range splitComma(*allow) {
		rule := egress.Rule{HostPattern: h}
		if host, portStr, err := net.SplitHostPort(h); err == nil {
			if port, err := strconv.Atoi(portStr); err == nil {
				rule = egress.Rule{HostPattern: host, Port: &port}
			}
		}
		al.AddRule(rule)
	}
	p := &egress.ForwardProxy{Default: al, Enforce: true, Guard: &egress.DialGuard{AllowLoopback: *allowLoopback}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("egress-proxy-smoke listening on %s allow=%q", *listen, *allow)
	if err := p.ListenAndServe(ctx, *listen); err != nil {
		log.Fatal(err)
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := trim(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
