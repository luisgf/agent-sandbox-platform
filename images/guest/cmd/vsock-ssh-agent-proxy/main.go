// Command vsock-ssh-agent-proxy exposes the host SSH agent inside the guest.
//
// Productive: dial AF_VSOCK CID 2 port 26501 (node-agent --host-vsock) and
// present a Unix socket at --listen (default /run/agent-sandbox/ssh-agent.sock).
//
// Lab/dry-run: --upstream unix:/path/to/host-vsock-26501.sock (or ASP_SSH_AGENT_UPSTREAM).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/mdlayher/vsock"
)

func main() {
	listen := flag.String("listen", getenv("ASP_SSH_AUTH_SOCK", "/run/agent-sandbox/ssh-agent.sock"), "unix socket path to expose (guest SSH_AUTH_SOCK)")
	cid := flag.Uint("cid", uint(getenvU32("ASP_HOST_CID", 2)), "host vsock CID (hypervisor is 2)")
	port := flag.Uint("port", uint(getenvU32("ASP_SSH_AGENT_VSOCK_PORT", 26501)), "host vsock SSH agent port")
	upstream := flag.String("upstream", os.Getenv("ASP_SSH_AGENT_UPSTREAM"), "optional upstream: unix:/path (lab) instead of vsock")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*listen), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	_ = os.Remove(*listen)
	ln, err := net.Listen("unix", *listen)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(*listen)
	}()
	_ = os.Chmod(*listen, 0o660)

	log.Printf("vsock-ssh-agent-proxy listening on %s → %s", *listen, describeUpstream(*upstream, uint32(*cid), uint32(*port)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		client, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("accept: %v", err)
				continue
			}
		}
		go handle(client, *upstream, uint32(*cid), uint32(*port))
	}
}

func describeUpstream(upstream string, cid, port uint32) string {
	if upstream != "" {
		return upstream
	}
	return fmt.Sprintf("vsock://%d:%d", cid, port)
}

func handle(client net.Conn, upstream string, cid, port uint32) {
	defer client.Close()
	up, err := dialUpstream(upstream, cid, port)
	if err != nil {
		log.Printf("dial upstream: %v", err)
		return
	}
	defer up.Close()
	pump(client, up)
}

func dialUpstream(upstream string, cid, port uint32) (net.Conn, error) {
	if upstream != "" {
		return net.Dial("unix", strings.TrimPrefix(upstream, "unix:"))
	}
	return vsock.Dial(cid, port, nil)
}

func pump(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvU32(k string, def uint32) uint32 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return def
	}
	return uint32(n)
}
