// Optional lab binary: HTTP forward proxy allow/deny smoke (deny-by-default).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/luisgf/agent-sandbox-platform/node-agent/internal/egress"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8888", "proxy listen address")
	allow := flag.String("allow", "127.0.0.1", "comma-separated allowlist hosts")
	flag.Parse()
	al := egress.NewAllowlist()
	for _, h := range splitComma(*allow) {
		al.Add(h)
	}
	p := &egress.ForwardProxy{Default: al, Enforce: true}
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
