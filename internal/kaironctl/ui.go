// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

package kaironctl

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// uiBaseURL is the kairon-ui address from KAIRON_UI_URL, without a trailing
// slash; "" when unset. Every kaironctl verb that talks to kairon-ui (image,
// network observability, packet capture) resolves it here.
func uiBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("KAIRON_UI_URL")), "/")
}

// uiToken is the bearer token for kairon-ui: KAIRON_UI_TOKEN, and, when
// allowConsole is set, KAIRON_CONSOLE_TOKEN as a fallback (the observability
// pass-through historically accepted the console token; other routes do not).
func uiToken(allowConsole bool) string {
	if t := os.Getenv("KAIRON_UI_TOKEN"); t != "" {
		return t
	}
	if allowConsole {
		return os.Getenv("KAIRON_CONSOLE_TOKEN")
	}
	return ""
}

// uiHTTPClient is the HTTP client for every call to kairon-ui. With
// KAIRON_UI_CA (a PEM file) it trusts that certificate or CA in addition to
// the system roots, which is how a dashboard served with a self-signed
// certificate (deploy-remote.sh --ui-tls) is reached without turning
// verification off. KAIRON_UI_INSECURE=true skips verification entirely; it
// exists for throwaway labs and is never the default.
func uiHTTPClient() (*http.Client, error) {
	ca := strings.TrimSpace(os.Getenv("KAIRON_UI_CA"))
	insecure := strings.EqualFold(strings.TrimSpace(os.Getenv("KAIRON_UI_INSECURE")), "true")
	if ca == "" && !insecure {
		return http.DefaultClient, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		cfg.InsecureSkipVerify = true // #nosec G402 -- explicit opt-in via KAIRON_UI_INSECURE
	}
	if ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("KAIRON_UI_CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("KAIRON_UI_CA %s: no PEM certificate found", ca)
		}
		cfg.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

// uiDo sends req with uiHTTPClient.
func uiDo(req *http.Request) (*http.Response, error) {
	c, err := uiHTTPClient()
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
