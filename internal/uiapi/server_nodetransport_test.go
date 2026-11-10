// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package uiapi

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestNodeTransportIsSharedWhenTLSConfigured(t *testing.T) {
	s := &Server{ConsoleTLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	a, b := s.nodeTransport(), s.nodeTransport()
	if a != b {
		t.Fatal("nodeTransport must return one shared transport, not a new one per call")
	}
	if a == http.DefaultTransport {
		t.Fatal("expected a dedicated transport carrying ConsoleTLS")
	}
	if a.(*http.Transport).TLSClientConfig != s.ConsoleTLS {
		t.Fatal("transport must use ConsoleTLS")
	}
}

func TestNodeTransportDefaultsWithoutTLS(t *testing.T) {
	if (&Server{}).nodeTransport() != http.DefaultTransport {
		t.Fatal("plaintext relay should use the default transport")
	}
}
