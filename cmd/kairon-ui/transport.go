// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/zyvorai/kairon/internal/tlsreload"
)

// transportMode is how kairon-ui is reached.
type transportMode int

const (
	// modeEphemeral serves HTTPS with a self-signed certificate generated at
	// startup. It is the default: no deployment path serves a login form or a
	// bearer token over plain HTTP unless an operator asks for it.
	modeEphemeral transportMode = iota
	// modeCertFiles serves HTTPS with the operator's -tls-cert/-tls-key,
	// reloaded when the files change.
	modeCertFiles
	// modeHTTP serves plain HTTP; only with -insecure-http.
	modeHTTP
)

func (m transportMode) scheme() string {
	if m == modeHTTP {
		return "http"
	}
	return "https"
}

// resolveTransport applies the all-or-nothing rule for -tls-cert/-tls-key
// (the same fail-closed posture as the OIDC flags) and the explicit opt-out.
// Giving a certificate and also asking for plain HTTP is a contradiction and
// a startup error, never a silent pick.
func resolveTransport(cert, key string, insecureHTTP bool) (transportMode, error) {
	haveCert, haveKey := cert != "", key != ""
	switch {
	case haveCert != haveKey:
		return 0, fmt.Errorf("-tls-cert and -tls-key must be set together (got cert=%q key=%q)", cert, key)
	case haveCert && insecureHTTP:
		return 0, fmt.Errorf("-insecure-http contradicts -tls-cert/-tls-key; set one or the other")
	case haveCert:
		return modeCertFiles, nil
	case insecureHTTP:
		return modeHTTP, nil
	}
	return modeEphemeral, nil
}

// newServerTLS loads the certificate once (a bad pair is a startup failure)
// and returns a TLS 1.2+ config that serves the most recently loaded pair.
func newServerTLS(log *slog.Logger, certPath, keyPath string) (*tls.Config, *tlsreload.Watcher, error) {
	w, err := tlsreload.New(log, certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load -tls-cert/-tls-key: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: w.GetCertificate}, w, nil
}

// ephemeralSANs lists what a self-signed certificate should be valid for:
// localhost, loopback, the hostname, every interface address, and any extra
// comma-separated names or IPs (KAIRON_UI_TLS_SANS), so a Service name or a
// load-balancer address can be covered.
func ephemeralSANs(extra string) ([]string, []net.IP) {
	dns := []string{"localhost", "kairon-ui"}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if h, err := os.Hostname(); err == nil && h != "" {
		dns = append(dns, h)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipn.IP)
			}
		}
	}
	for _, s := range strings.Split(extra, ",") {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
		case net.ParseIP(s) != nil:
			ips = append(ips, net.ParseIP(s))
		default:
			dns = append(dns, s)
		}
	}
	return uniqueStrings(dns), uniqueIPs(ips)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func uniqueIPs(in []net.IP) []net.IP {
	seen := map[string]bool{}
	var out []net.IP
	for _, ip := range in {
		if k := ip.String(); !seen[k] {
			seen[k] = true
			out = append(out, ip)
		}
	}
	return out
}

// newEphemeralTLS generates an in-memory ECDSA P-256 self-signed certificate
// and returns a TLS 1.2+ config serving it, plus the parsed certificate.
// It changes on every restart (and differs per replica), which is why a
// stable certificate (-tls-cert/-tls-key, or ui.tls.secretName) is better
// for anything beyond a lab.
func newEphemeralTLS(extraSANs string) (*tls.Config, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	dns, ips := ephemeralSANs(extraSANs)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "kairon-ui", Organization: []string{"Kairon (self-signed)"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, leaf, nil
}

// isSelfSigned reports whether c verifies against its own public key, which
// is what makes a verifying client (kairon-node downloading an uploaded
// image) need either a trust anchor or the digest-only escape hatch.
func isSelfSigned(c *x509.Certificate) bool {
	if c == nil || string(c.RawIssuer) != string(c.RawSubject) {
		return false
	}
	// CheckSignatureFrom would also demand the certificate be a CA; what
	// matters here is only that it was signed by its own key.
	return c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

// leafFromFile parses the first certificate in a PEM file.
func leafFromFile(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("%s: no certificate found", path)
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}
