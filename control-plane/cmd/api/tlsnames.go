package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// tlsCertNames returns the names a TLS certificate file is valid for: the DNS
// names, the IP addresses and the common name of its first certificate. A node
// may not enroll under any of them.
func tlsCertNames(path string) ([]string, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s holds no PEM certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	var names []string
	names = append(names, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		names = append(names, ip.String())
	}
	if cert.Subject.CommonName != "" {
		names = append(names, cert.Subject.CommonName)
	}
	return names, nil
}
