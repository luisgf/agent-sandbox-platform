package main

import "net"

// defaultIssuer is the OIDC issuer when ASP_OIDC_ISSUER names none: this control plane,
// at the address it listens on. An address with no host, or the unspecified one, is
// reached over the loopback. (It used to be "http://127.0.0.1" + addr, which is only a
// URL for ":8080": the default 127.0.0.1:8080 became http://127.0.0.1127.0.0.1:8080.)
// A control plane that other hosts verify tokens against sets ASP_OIDC_ISSUER.
func defaultIssuer(addr string, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return scheme + "://" + addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}
